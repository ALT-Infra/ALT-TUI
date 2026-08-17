package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"
	"altv1/internal/tooling"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

type sessionRuntime struct {
	store         *store.Store
	providers     *provider.Registry
	session       *store.Session
	document      *profile.Document
	profile       profile.Profile
	run           *Run
	tools         *tooling.Runtime
	engineOptions EngineOptions

	mu                  sync.Mutex
	surfaceLocksMu      sync.Mutex
	surfaceLocks        map[string]*sync.Mutex
	signalMu            sync.Mutex
	loop                *adk.TurnLoop[Signal, *schema.Message]
	pending             []Signal
	active              map[string]context.CancelFunc
	launched            map[string]struct{}
	conversationHistory []ConversationTurn
}

func newSessionRuntime(
	ledger *store.Store,
	providers *provider.Registry,
	session *store.Session,
	document *profile.Document,
	run *Run,
) *sessionRuntime {
	return &sessionRuntime{
		store:     ledger,
		providers: providers,
		session:   session,
		document:  document,
		profile:   document.Profile,
		run:       run,
		active:    make(map[string]context.CancelFunc),
		launched:  make(map[string]struct{}),
	}
}

func (r *sessionRuntime) execute(parent context.Context, recovered bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	runtimeTools, err := tooling.NewRuntimeWithOptions(
		ctx,
		r.session.Workspace,
		tooling.RuntimeOptions{
			DangerouslyBypassApprovalsAndSandbox: r.engineOptions.DangerouslyBypassApprovalsAndSandbox,
			SensitiveEnvironment:                 r.engineOptions.SensitiveEnvironment,
			PersistReasoning:                     r.profile.Policy.PersistReasoning,
			ContextArchiveDirectory:              requestContextArchive(r.engineOptions.ContextArchiveRoot, r.session.ID),
			SearchContext:                        r.searchContext,
			BrowseContext:                        r.browseContext,
			OpenContext:                          r.openContext,
			ArchiveToolOutput:                    r.archiveToolOutput,
			RecordAgentCompaction:                r.recordAgentCompaction,
			ResolveResearchProvider:              r.engineOptions.ResolveResearchProvider,
			ResolveExaCredential:                 r.engineOptions.ResolveExaCredential,
			ResolveLinkupCredential:              r.engineOptions.ResolveLinkupCredential,
		},
	)
	if err != nil {
		return err
	}
	r.tools = runtimeTools
	defer runtimeTools.Close()

	if recovered {
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.SessionRecovered, Actor: "system",
		}); err != nil {
			return err
		}
	}
	if err := r.loadConversationHistory(ctx); err != nil {
		return err
	}
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	if state.Terminal {
		return nil
	}
	if state.LeaderID == "" {
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.LeadershipTransferred, Actor: "system",
			Data: event.LeadershipTransferredData{
				ToAgentID: r.profile.Primary.ID,
				Reason:    "Every user turn enters through the Team primary.",
			},
		}); err != nil {
			return r.fail(ctx, err)
		}
	}
	if recovered {
		if err := r.markInterruptedDelegations(ctx); err != nil {
			return r.fail(ctx, err)
		}
	}

	loop := adk.NewTurnLoop(adk.TurnLoopConfig[Signal, *schema.Message]{
		GenInput: func(_ context.Context, _ *adk.TurnLoop[Signal, *schema.Message], items []Signal) (*adk.GenInputResult[Signal, *schema.Message], error) {
			return &adk.GenInputResult[Signal, *schema.Message]{
				Input:    &adk.AgentInput{},
				Consumed: append([]Signal(nil), items...),
			}, nil
		},
		GenResume: func(
			_ context.Context,
			_ *adk.TurnLoop[Signal, *schema.Message],
			interrupted []Signal,
			unhandled []Signal,
			newItems []Signal,
		) (*adk.GenResumeResult[Signal, *schema.Message], error) {
			all := append(append(append([]Signal(nil), interrupted...), unhandled...), newItems...)
			return &adk.GenResumeResult[Signal, *schema.Message]{Consumed: all}, nil
		},
		PrepareAgent: func(_ context.Context, _ *adk.TurnLoop[Signal, *schema.Message], consumed []Signal) (adk.Agent, error) {
			return &activeTurnAgent{runtime: r, signals: append([]Signal(nil), consumed...)}, nil
		},
		OnAgentEvents: r.onActiveAgentEvents,
		Store:         r.store,
		CheckpointID:  "active-agent:" + r.session.ID,
	})

	initial := Signal{Kind: "session.ready"}
	if recovered {
		initial.Kind = "session.recovered"
	}
	r.installLoop(loop)
	r.pushSignal(initial)
	loop.Run(ctx)
	if err := r.scheduleReady(ctx); err != nil {
		loop.Stop()
		return r.fail(ctx, err)
	}

	exit := loop.Wait()
	r.cancelActive()
	state, stateErr := r.projection(context.WithoutCancel(ctx))
	if stateErr == nil && state.Terminal {
		return nil
	}
	if exit.ExitReason != nil {
		if terminalContextError(exit.ExitReason) || terminalContextError(ctx.Err()) {
			if r.run.userCancelled.Load() {
				return nil
			}
			// Application shutdown leaves the session running for recovery.
			return ctx.Err()
		}
		return r.fail(context.WithoutCancel(ctx), exit.ExitReason)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func requestContextArchive(root, sessionID string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return filepath.Join(root, "sessions", sessionID)
}

func (r *sessionRuntime) installLoop(loop *adk.TurnLoop[Signal, *schema.Message]) {
	r.signalMu.Lock()
	r.loop = loop
	pending := append([]Signal(nil), r.pending...)
	r.pending = nil
	r.signalMu.Unlock()
	for _, signal := range pending {
		loop.Push(signal)
	}
}

func (r *sessionRuntime) pushSignal(signal Signal) bool {
	r.signalMu.Lock()
	loop := r.loop
	if loop == nil {
		r.pending = append(r.pending, signal)
		r.signalMu.Unlock()
		return true
	}
	r.signalMu.Unlock()
	accepted, _ := loop.Push(signal)
	return accepted
}

func (r *sessionRuntime) loadConversationHistory(ctx context.Context) error {
	turns, err := r.store.ConversationSessions(ctx, r.session.ID)
	if err != nil {
		return fmt.Errorf("load conversation history: %w", err)
	}
	prior := make([]store.Session, 0, max(0, len(turns)-1))
	for _, turn := range turns {
		if turn.ID == r.session.ID {
			break
		}
		prior = append(prior, turn)
	}
	history := make([]ConversationTurn, 0, len(prior))
	for _, turn := range prior {
		entry := ConversationTurn{SessionID: turn.ID, Status: string(turn.Status), LeaderID: turn.LeaderID}
		items, err := r.store.Events(ctx, turn.ID, 0)
		if err != nil {
			return fmt.Errorf("load conversation turn %s provenance: %w", turn.ID, err)
		}
		entry.Task = turn.Task
		entry.Answer = turn.FinalAnswer
		for _, item := range items {
			switch item.Kind {
			case event.SessionCreated:
				entry.TaskReference = store.ContextReferenceForEvent(item)
			case event.FinalCompleted:
				entry.AnswerReference = store.ContextReferenceForEvent(item)
			}
			if !sharedConversationEvent(item.Kind) {
				continue
			}
			entry.ObservableTrace = append(entry.ObservableTrace, ConversationTrace{
				Reference: store.ContextReferenceForEvent(item),
				Sequence:  item.Sequence, Kind: item.Kind, Actor: item.Actor,
				CorrelationID: item.CorrelationID,
				Data:          append([]byte(nil), item.Data...),
			})
		}
		history = append(history, entry)
	}
	r.conversationHistory = history
	return nil
}

func sharedConversationEvent(kind event.Kind) bool {
	switch kind {
	case event.UserInstruction,
		event.LeadershipTransferred,
		event.AgentDecision,
		event.DelegationCreated,
		event.DelegationStarted,
		event.DelegationReasoning,
		event.DelegationCompleted,
		event.DelegationFailed,
		event.DelegationCancelled,
		event.PeerTurnCreated,
		event.PeerTurnStarted,
		event.PeerReasoning,
		event.PeerTurnCompleted,
		event.PeerTurnFailed,
		event.PeerTurnCancelled,
		event.ToolCalled,
		event.ToolCompleted,
		event.FinalReasoning,
		event.FinalCompleted,
		event.SessionFailed,
		event.SessionCancelled:
		return true
	default:
		return false
	}
}

func (r *sessionRuntime) onActiveAgentEvents(
	ctx context.Context,
	turn *adk.TurnContext[Signal, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent],
) error {
	var outcome *AgentOutcome
	for {
		item, ok := events.Next()
		if !ok {
			break
		}
		if item.Err != nil {
			return item.Err
		}
		if item.Output == nil || item.Output.CustomizedOutput == nil {
			continue
		}
		value, ok := item.Output.CustomizedOutput.(AgentOutcome)
		if !ok {
			return fmt.Errorf("active agent returned unsupported output %T", item.Output.CustomizedOutput)
		}
		outcome = &value
	}
	if outcome == nil {
		return fmt.Errorf("active agent turn returned no outcome")
	}
	if strings.TrimSpace(outcome.Answer) != "" {
		state, err := r.projection(ctx)
		if err != nil {
			return err
		}
		if err := r.completeAnswer(ctx, state.LeaderID, state.AgentTurns, outcome.Answer); err != nil {
			return err
		}
		turn.Loop.Stop()
		return nil
	}
	if outcome.Decision == nil {
		return fmt.Errorf("active agent returned neither an answer nor a coordination decision")
	}
	finalized, err := r.applyDecision(ctx, *outcome.Decision)
	if err != nil {
		return err
	}
	if finalized {
		turn.Loop.Stop()
	}
	return nil
}

func (r *sessionRuntime) runActiveAgent(ctx context.Context, signals []Signal) (AgentOutcome, error) {
	state, err := r.projection(ctx)
	if err != nil {
		return AgentOutcome{}, err
	}
	agent, ok := r.profile.Agent(state.LeaderID)
	if !ok {
		return AgentOutcome{}, fmt.Errorf("current leader %s is absent from pinned profile", state.LeaderID)
	}
	turn := state.AgentTurns + 1
	kinds := make([]string, 0, len(signals))
	for _, signal := range signals {
		kinds = append(kinds, signal.Kind)
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.AgentTurnStarted, Actor: agent.ID,
		Data: event.AgentTurnData{AgentID: agent.ID, Turn: turn, SignalKinds: kinds},
	}); err != nil {
		return AgentOutcome{}, err
	}
	unlockSurface := r.lockModelSurface(agent.ID)
	defer unlockSurface()
	surface, err := loadModelSurface(ctx, r.store, r.session.ConversationID, agent.ID)
	if err != nil {
		return AgentOutcome{}, err
	}
	system, workingView := activeAgentMessages(
		r.profile, agent, state, signals, surface.LastSessionID != r.session.ID,
	)
	exactUserMessage, err := r.richExactInputMessage(ctx, agent.Model, state.TaskInput)
	if err != nil {
		return AgentOutcome{}, err
	}
	if _, err := r.commitWorkingView(ctx, "agent", agent.ID, state.LastSequence, workingView); err != nil {
		return AgentOutcome{}, err
	}
	outcome, err := r.generateAgentOutcome(ctx, agent, turn, system, exactUserMessage, workingView, state.WorkCount(), surface)
	if err != nil {
		return AgentOutcome{}, fmt.Errorf("agent %s turn %d: %w", agent.ID, turn, err)
	}
	if outcome.Decision != nil {
		outcome.Decision.observedWork = state.WorkCount()
	}
	return outcome, nil
}

func (r *sessionRuntime) applyDecision(ctx context.Context, decision AgentDecision) (bool, error) {
	state, err := r.projection(ctx)
	if err != nil {
		return false, err
	}
	agent, ok := r.profile.Agent(state.LeaderID)
	if !ok {
		return false, fmt.Errorf("current leader %s is absent", state.LeaderID)
	}

	if decision.Handoff != nil && (len(decision.Delegations) > 0 || len(decision.PeerTurns) > 0) {
		return false, fmt.Errorf("leadership handoff must be an exclusive coordination action")
	}
	cancellations := uniqueStrings(decision.Cancel)
	for _, id := range cancellations {
		if state.Delegations[id] == nil && state.PeerTurns[id] == nil {
			return false, fmt.Errorf("cannot cancel unknown work %s", id)
		}
	}
	var handoffPeer profile.AgentAssignment
	handoffReason := ""
	if decision.Handoff != nil {
		handoffPeer, ok = r.profile.PeerAgentFor(agent, strings.TrimSpace(decision.Handoff.PeerID))
		if !ok {
			return false, fmt.Errorf("agent %s cannot hand leadership to %s", agent.ID, decision.Handoff.PeerID)
		}
		handoffReason = strings.TrimSpace(decision.Handoff.Reason)
		if handoffReason == "" {
			return false, fmt.Errorf("leadership handoff requires a reason")
		}
	}
	specs, err := r.materializeDelegations(state, agent, decision.Delegations)
	if err != nil {
		return false, err
	}
	for _, spec := range specs {
		for _, dependency := range spec.DependsOn {
			if containsString(cancellations, dependency) {
				return false, fmt.Errorf("delegation %s depends on work %s cancelled by the same decision", spec.Key, dependency)
			}
		}
	}
	peerSpecs, err := r.materializePeerTurns(state, agent, decision.PeerTurns)
	if err != nil {
		return false, err
	}
	if decision.Handoff == nil && len(specs) == 0 && len(peerSpecs) == 0 && decision.observedWork == 0 {
		current, err := r.projection(ctx)
		if err != nil {
			return false, err
		}
		if current.WorkCount() == 0 {
			return false, fmt.Errorf(
				"active agent produced no work, handoff, or user answer while no work was active",
			)
		}
	}
	for _, id := range cancellations {
		if err := r.cancelWork(ctx, id, "cancelled by active-agent coordination decision"); err != nil {
			return false, err
		}
	}
	_, err = r.store.Append(ctx, r.session.ID, event.Draft{
		Kind:  event.AgentDecision,
		Actor: agent.ID,
		Data: event.AgentDecisionData{
			AgentID:       agent.ID,
			Turn:          state.AgentTurns,
			Assessment:    decision.Assessment,
			Delegations:   specs,
			PeerTurns:     peerSpecs,
			Cancellations: cancellations,
			HandoffTo:     handoffPeerID(decision.Handoff),
		},
	})
	if err != nil {
		return false, err
	}
	for _, spec := range specs {
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.DelegationCreated, Actor: agent.ID,
			CorrelationID: spec.ID, Data: spec,
		}); err != nil {
			return false, err
		}
	}
	for _, spec := range peerSpecs {
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.PeerTurnCreated, Actor: agent.ID,
			CorrelationID: spec.ID, Data: spec,
		}); err != nil {
			return false, err
		}
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.AgentTurnCompleted, Actor: agent.ID,
		Data: event.AgentTurnData{AgentID: agent.ID, Turn: state.AgentTurns, Assessment: decision.Assessment},
	}); err != nil {
		return false, err
	}

	if decision.Handoff != nil {
		transferred, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.LeadershipTransferred, Actor: agent.ID,
			Data: event.LeadershipTransferredData{FromAgentID: agent.ID, ToAgentID: handoffPeer.ID, Reason: handoffReason},
		})
		if err != nil {
			return false, err
		}
		r.pushSignal(Signal{Kind: string(event.LeadershipTransferred), EventID: transferred.ID})
		return false, nil
	}
	if len(cancellations) > 0 && len(specs) == 0 && len(peerSpecs) == 0 {
		r.pushSignal(Signal{Kind: "work.cancelled"})
	}
	if err := r.scheduleReady(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (r *sessionRuntime) materializePeerTurns(
	state *Projection,
	agent profile.AgentAssignment,
	proposals []ProposedPeerTurn,
) ([]event.PeerTurnSpec, error) {
	type collaboration struct {
		callerID string
		peerID   string
		round    int
		active   bool
	}
	known := make(map[string]collaboration)
	for _, turn := range state.SortedPeerTurns() {
		current := known[turn.Spec.CollaborationID]
		if current.peerID == "" {
			current.peerID = turn.Spec.PeerID
			current.callerID = turn.Spec.CallerID
		}
		if turn.Spec.Round > current.round {
			current.round = turn.Spec.Round
		}
		current.active = current.active || turn.Status == DelegationPending || turn.Status == DelegationRunning
		known[turn.Spec.CollaborationID] = current
	}
	keys := make(map[string]bool)
	usedCollaborations := make(map[string]bool)
	var result []event.PeerTurnSpec
	for index, proposal := range proposals {
		key := strings.TrimSpace(proposal.Key)
		if key == "" {
			return nil, fmt.Errorf("peer turn %d requires a key", index)
		}
		if keys[key] {
			return nil, fmt.Errorf("peer turn key %q is duplicated", key)
		}
		keys[key] = true
		peer, ok := r.profile.PeerAgentFor(agent, proposal.PeerID)
		if !ok {
			return nil, fmt.Errorf("agent %s has no peer relationship with %s", agent.ID, proposal.PeerID)
		}
		if strings.TrimSpace(proposal.Objective) == "" {
			return nil, fmt.Errorf("peer turn %s has an empty objective", key)
		}
		attachments, err := validateAttachmentSelection(state, proposal.Attachments)
		if err != nil {
			return nil, fmt.Errorf("peer turn %s: %w", key, err)
		}
		collaborationID := strings.TrimSpace(proposal.CollaborationID)
		if collaborationID == "" {
			id, err := uuid.NewV7()
			if err != nil {
				return nil, fmt.Errorf("create collaboration id: %w", err)
			}
			collaborationID = id.String()
		}
		current, exists := known[collaborationID]
		if exists && (current.peerID != peer.ID || current.callerID != agent.ID) {
			return nil, fmt.Errorf("collaboration %s belongs to %s consulting %s", collaborationID, current.callerID, current.peerID)
		}
		if current.active || usedCollaborations[collaborationID] {
			return nil, fmt.Errorf("collaboration %s already has an active or newly planned turn; wait for it before continuing", collaborationID)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("create peer turn id: %w", err)
		}
		spec := event.PeerTurnSpec{
			ID: id.String(), Key: key, CollaborationID: collaborationID,
			PeerID: peer.ID, CallerID: agent.ID, Objective: strings.TrimSpace(proposal.Objective),
			Context:     strings.TrimSpace(proposal.Context),
			Attachments: attachments,
			Round:       current.round + 1,
		}
		usedCollaborations[collaborationID] = true
		known[collaborationID] = collaboration{callerID: agent.ID, peerID: peer.ID, round: spec.Round, active: true}
		result = append(result, spec)
	}
	return result, nil
}

func (r *sessionRuntime) materializeDelegations(
	state *Projection,
	agent profile.AgentAssignment,
	proposals []ProposedDelegation,
) ([]event.DelegationSpec, error) {
	known := make(map[string]event.DelegationSpec, len(state.Delegations)+len(proposals))
	for id, delegation := range state.Delegations {
		known[id] = delegation.Spec
	}
	keys := make(map[string]string)
	var result []event.DelegationSpec
	for i, proposal := range proposals {
		if strings.TrimSpace(proposal.Key) == "" {
			return nil, fmt.Errorf("delegation %d requires a key", i)
		}
		if _, exists := keys[proposal.Key]; exists {
			return nil, fmt.Errorf("delegation key %q is duplicated", proposal.Key)
		}
		_, ok := r.profile.SpecialistFor(agent, proposal.SpecialistID)
		if !ok {
			return nil, fmt.Errorf("agent %s cannot access specialist %s", agent.ID, proposal.SpecialistID)
		}
		if strings.TrimSpace(proposal.Objective) == "" {
			return nil, fmt.Errorf("delegation %s has an empty objective", proposal.Key)
		}
		attachments, err := validateAttachmentSelection(state, proposal.Attachments)
		if err != nil {
			return nil, fmt.Errorf("delegation %s: %w", proposal.Key, err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("create delegation id: %w", err)
		}
		dependencies := make([]string, 0, len(proposal.DependsOn))
		depth := 1
		for _, reference := range uniqueStrings(proposal.DependsOn) {
			dependencyID := reference
			if alias, ok := keys[reference]; ok {
				dependencyID = alias
			}
			dependency, ok := known[dependencyID]
			if !ok {
				return nil, fmt.Errorf("delegation %s depends on unknown or later work %s", proposal.Key, reference)
			}
			dependencies = append(dependencies, dependencyID)
			if dependency.Depth+1 > depth {
				depth = dependency.Depth + 1
			}
		}
		spec := event.DelegationSpec{
			ID:           id.String(),
			Key:          proposal.Key,
			SpecialistID: proposal.SpecialistID,
			CallerID:     agent.ID,
			Objective:    strings.TrimSpace(proposal.Objective),
			Context:      strings.TrimSpace(proposal.Context),
			Attachments:  attachments,
			DependsOn:    dependencies,
			Depth:        depth,
		}
		keys[proposal.Key] = spec.ID
		known[spec.ID] = spec
		result = append(result, spec)
	}
	return result, nil
}

func (r *sessionRuntime) scheduleReady(ctx context.Context) error {
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	for _, delegation := range state.SortedDelegations() {
		if delegation.Status == DelegationCompleted || delegation.Status == DelegationCancelled ||
			delegation.Status == DelegationRunning {
			continue
		}
		if delegation.Status == DelegationFailed {
			if !delegation.Interrupted {
				continue
			}
		}
		if !state.DependenciesCompleted(delegation.Spec) {
			continue
		}
		attempt := delegation.Attempt + 1
		key := fmt.Sprintf("%s:%d", delegation.Spec.ID, attempt)
		r.mu.Lock()
		if _, exists := r.launched[key]; exists {
			r.mu.Unlock()
			continue
		}
		r.launched[key] = struct{}{}
		r.mu.Unlock()
		go r.executeDelegation(ctx, delegation.Spec.ID, attempt)
	}
	for _, turn := range state.SortedPeerTurns() {
		if turn.Status == DelegationCompleted || turn.Status == DelegationCancelled || turn.Status == DelegationRunning {
			continue
		}
		if turn.Status == DelegationFailed && !turn.Interrupted {
			continue
		}
		attempt := turn.Attempt + 1
		key := fmt.Sprintf("peer:%s:%d", turn.Spec.ID, attempt)
		r.mu.Lock()
		if _, exists := r.launched[key]; exists {
			r.mu.Unlock()
			continue
		}
		r.launched[key] = struct{}{}
		r.mu.Unlock()
		go r.executePeerTurn(ctx, turn.Spec.ID, attempt)
	}
	return nil
}

func (r *sessionRuntime) executeDelegation(parent context.Context, delegationID string, attempt int) {
	state, err := r.projection(parent)
	if err != nil {
		r.stopWithError(err)
		return
	}
	delegation := state.Delegations[delegationID]
	if delegation == nil || delegation.Status == DelegationCompleted || delegation.Status == DelegationCancelled {
		return
	}
	caller, ok := r.profile.Agent(delegation.Spec.CallerID)
	if !ok {
		r.stopWithError(fmt.Errorf("delegation caller %s is absent", delegation.Spec.CallerID))
		return
	}
	specialist, ok := r.profile.SpecialistFor(caller, delegation.Spec.SpecialistID)
	if !ok {
		r.stopWithError(fmt.Errorf("specialist %s is not permitted", delegation.Spec.SpecialistID))
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.active[delegationID] = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, delegationID)
		r.mu.Unlock()
	}()

	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.DelegationStarted, Actor: specialist.ID,
		CorrelationID: delegationID,
		Data:          event.DelegationStartedData{DelegationID: delegationID, Attempt: attempt},
	}); err != nil {
		r.stopWithError(err)
		return
	}
	var raw strings.Builder
	rawResult, runErr := r.runSpecialist(ctx, specialist, delegation, attempt)
	raw.WriteString(rawResult)
	if runErr != nil {
		r.handleDelegationFailure(parent, delegationID, specialist.ID, attempt, runErr)
		return
	}
	result, parseErr := decodeJSONObject[SpecialistResult](raw.String())
	if parseErr != nil {
		result = SpecialistResult{Result: strings.TrimSpace(raw.String())}
	}
	if strings.TrimSpace(result.Result) == "" {
		r.handleDelegationFailure(parent, delegationID, specialist.ID, attempt, fmt.Errorf("specialist returned an empty result"))
		return
	}
	completed, err := r.store.Append(parent, r.session.ID, event.Draft{
		Kind: event.DelegationCompleted, Actor: specialist.ID,
		CorrelationID: delegationID,
		Data: event.DelegationCompletedData{
			DelegationID: delegationID,
			Attempt:      attempt,
			Result:       result.Result,
			Findings:     result.Findings,
			Risks:        result.Risks,
			Confidence:   result.Confidence,
		},
	})
	if err != nil {
		r.stopWithError(err)
		return
	}
	r.pushSignal(Signal{Kind: string(event.DelegationCompleted), EventID: completed.ID, DelegationID: delegationID})
	if err := r.scheduleReady(parent); err != nil {
		r.stopWithError(err)
	}
}

func (r *sessionRuntime) handleDelegationFailure(
	ctx context.Context,
	delegationID string,
	actor string,
	attempt int,
	cause error,
) {
	state, err := r.projection(context.WithoutCancel(ctx))
	if err == nil {
		if delegation := state.Delegations[delegationID]; delegation != nil && delegation.Status == DelegationCancelled {
			return
		}
	}
	if errors.Is(cause, context.Canceled) && !r.run.userCancelled.Load() {
		// Process shutdown is not a semantic failure. Leave the durable state as
		// running so recovery can classify and retry the interrupted attempt.
		return
	}
	failed, err := r.store.Append(context.WithoutCancel(ctx), r.session.ID, event.Draft{
		Kind: event.DelegationFailed, Actor: actor, CorrelationID: delegationID,
		Data: event.DelegationFailedData{
			DelegationID: delegationID, Attempt: attempt,
			Error: cause.Error(),
		},
	})
	if err != nil {
		r.stopWithError(err)
		return
	}
	r.pushSignal(Signal{Kind: string(event.DelegationFailed), EventID: failed.ID, DelegationID: delegationID})
}

func (r *sessionRuntime) completeAnswer(ctx context.Context, agentID string, turn int, answer string) error {
	r.run.finalizing.Store(true)
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return fmt.Errorf("active agent returned an empty user answer")
	}
	r.cancelActiveDelegations(ctx, "current leader answered the user")
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.AgentTurnCompleted, Actor: agentID,
		Data: event.AgentTurnData{AgentID: agentID, Turn: turn, Assessment: "Answered the user directly."},
	}); err != nil {
		return err
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalStarted, Actor: agentID,
	}); err != nil {
		return err
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalTextDelta, Actor: agentID,
		Data: event.TextDeltaData{Text: answer},
	}); err != nil {
		return err
	}
	_, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalCompleted, Actor: agentID,
		Data: event.FinalCompletedData{Answer: answer},
	})
	return err
}

func validateAttachmentSelection(state *Projection, requested []string) ([]string, error) {
	available := make(map[string]bool)
	for _, reference := range projectionAttachmentReferences(state) {
		available[reference] = true
	}
	selected := uniqueStrings(requested)
	for _, reference := range selected {
		if !available[reference] {
			return nil, fmt.Errorf("attachment %s is not available in this conversation turn", reference)
		}
	}
	return selected, nil
}

func (r *sessionRuntime) markInterruptedDelegations(ctx context.Context) error {
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	for _, delegation := range state.SortedDelegations() {
		if delegation.Status != DelegationRunning {
			continue
		}
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.DelegationFailed, Actor: "recovery",
			CorrelationID: delegation.Spec.ID,
			Data: event.DelegationFailedData{
				DelegationID: delegation.Spec.ID,
				Attempt:      delegation.Attempt,
				Error:        "execution interrupted before a durable terminal event",
			},
		}); err != nil {
			return err
		}
	}
	for _, turn := range state.SortedPeerTurns() {
		if turn.Status != DelegationRunning {
			continue
		}
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.PeerTurnFailed, Actor: "recovery", CorrelationID: turn.Spec.ID,
			Data: event.PeerTurnFailedData{PeerTurnID: turn.Spec.ID, Attempt: turn.Attempt, Error: "execution interrupted before a durable terminal event"},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRuntime) cancelWork(ctx context.Context, id, reason string) error {
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	if state.Delegations[id] != nil {
		return r.cancelDelegation(ctx, id, reason)
	}
	turn := state.PeerTurns[id]
	if turn == nil {
		return fmt.Errorf("cannot cancel unknown work %s", id)
	}
	if turn.Status == DelegationCompleted || turn.Status == DelegationCancelled {
		return nil
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.PeerTurnCancelled, Actor: state.LeaderID, CorrelationID: id,
		Data: event.PeerTurnCancelledData{PeerTurnID: id, Reason: reason},
	}); err != nil {
		return err
	}
	r.mu.Lock()
	cancel := r.active[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *sessionRuntime) cancelDelegation(ctx context.Context, id, reason string) error {
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	delegation := state.Delegations[id]
	if delegation == nil {
		return fmt.Errorf("cannot cancel unknown delegation %s", id)
	}
	if delegation.Status == DelegationCompleted || delegation.Status == DelegationCancelled {
		return nil
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.DelegationCancelled, Actor: state.LeaderID,
		CorrelationID: id,
		Data:          event.DelegationCancelledData{DelegationID: id, Reason: reason},
	}); err != nil {
		return err
	}
	r.mu.Lock()
	cancel := r.active[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *sessionRuntime) cancelActiveDelegations(ctx context.Context, reason string) {
	state, err := r.projection(ctx)
	if err != nil {
		return
	}
	for _, delegation := range state.SortedDelegations() {
		if delegation.Status == DelegationPending || delegation.Status == DelegationRunning {
			_ = r.cancelDelegation(ctx, delegation.Spec.ID, reason)
		}
	}
	for _, turn := range state.SortedPeerTurns() {
		if turn.Status == DelegationPending || turn.Status == DelegationRunning {
			_ = r.cancelWork(ctx, turn.Spec.ID, reason)
		}
	}
}

func (r *sessionRuntime) cancelActive() {
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.active))
	for _, cancel := range r.active {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (r *sessionRuntime) projection(ctx context.Context) (*Projection, error) {
	events, err := r.store.Events(ctx, r.session.ID, 0)
	if err != nil {
		return nil, err
	}
	state, err := Replay(r.session.ID, events)
	if err != nil {
		return nil, err
	}
	state.ConversationHistory = append([]ConversationTurn(nil), r.conversationHistory...)
	return state, nil
}

func (r *sessionRuntime) stopWithError(err error) {
	if err == nil || r.loop == nil {
		return
	}
	// A caller or process shutdown is not a semantic session failure. Durable
	// running work is intentionally left for recovery to classify and retry.
	if terminalContextError(err) {
		return
	}
	_ = r.fail(context.Background(), err)
	r.loop.Stop(adk.WithImmediate())
}

func (r *sessionRuntime) fail(ctx context.Context, cause error) error {
	state, err := r.projection(ctx)
	if err == nil && !state.Terminal {
		_, _ = r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.SessionFailed, Actor: "system",
			Data: event.FailureData{Error: cause.Error()},
		})
	}
	return cause
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func handoffPeerID(handoff *ProposedHandoff) string {
	if handoff == nil {
		return ""
	}
	return strings.TrimSpace(handoff.PeerID)
}
