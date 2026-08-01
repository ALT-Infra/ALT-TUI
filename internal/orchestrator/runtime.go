package orchestrator

import (
	"context"
	"errors"
	"fmt"
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
			ResolveExaCredential:                 r.engineOptions.ResolveExaCredential,
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
	if state.LeadID == "" {
		if err := r.route(ctx); err != nil {
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
			return &leadTurnAgent{runtime: r, signals: append([]Signal(nil), consumed...)}, nil
		},
		OnAgentEvents: r.onLeadEvents,
		Store:         r.store,
		CheckpointID:  "lead:" + r.session.ID,
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
	history := make([]ConversationTurn, 0, max(0, len(turns)-1))
	for _, turn := range turns {
		if turn.ID == r.session.ID {
			break
		}
		items, err := r.store.Events(ctx, turn.ID, 0)
		if err != nil {
			return fmt.Errorf("load conversation turn %s provenance: %w", turn.ID, err)
		}
		entry := ConversationTurn{
			Task: turn.Task, Answer: turn.FinalAnswer, Status: string(turn.Status),
			LeadID: turn.LeadID,
		}
		for _, item := range items {
			if !sharedConversationEvent(item.Kind) {
				continue
			}
			entry.ObservableTrace = append(entry.ObservableTrace, ConversationTrace{
				Sequence: item.Sequence, Kind: item.Kind, Actor: item.Actor,
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
		event.LeadSelected,
		event.LeadDecision,
		event.DelegationCreated,
		event.DelegationStarted,
		event.DelegationReasoning,
		event.DelegationCompleted,
		event.DelegationFailed,
		event.DelegationCancelled,
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

func (r *sessionRuntime) route(ctx context.Context) error {
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.RouterStarted, Actor: "router",
	}); err != nil {
		return err
	}
	system, user := routerMessages(r.profile, r.session.Task, r.conversationHistory)
	decision, err := generateStructured[RouterDecision](
		ctx, r.session.ID, r.store, r.providers, r.profile,
		r.profile.Router.Model, "router", system, user,
		func(decision RouterDecision) error {
			if _, ok := r.profile.Lead(decision.LeadID); !ok {
				return fmt.Errorf("selected ineligible Lead %q", decision.LeadID)
			}
			if decision.Confidence < 0 || decision.Confidence > 1 {
				return fmt.Errorf("confidence %.3f is outside [0,1]", decision.Confidence)
			}
			if strings.TrimSpace(decision.Basis) == "" {
				return fmt.Errorf("decision basis is required")
			}
			return nil
		},
		r.observeModel(r.profile.Router.Model, "router"),
	)
	if err != nil {
		return fmt.Errorf("route task: %w", err)
	}
	if _, ok := r.profile.Lead(decision.LeadID); !ok {
		return fmt.Errorf("router selected ineligible Lead %q", decision.LeadID)
	}
	if decision.Confidence < 0 || decision.Confidence > 1 {
		return fmt.Errorf("router confidence %.3f is outside [0,1]", decision.Confidence)
	}
	if strings.TrimSpace(decision.Basis) == "" {
		return fmt.Errorf("router omitted its decision basis")
	}
	_, err = r.store.Append(ctx, r.session.ID, event.Draft{
		Kind:  event.LeadSelected,
		Actor: "router",
		Data: event.LeadSelectedData{
			LeadID: decision.LeadID, Confidence: decision.Confidence, Basis: decision.Basis,
		},
	})
	return err
}

func (r *sessionRuntime) onLeadEvents(
	ctx context.Context,
	turn *adk.TurnContext[Signal, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent],
) error {
	var decision *LeadDecision
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
		value, ok := item.Output.CustomizedOutput.(LeadDecision)
		if !ok {
			return fmt.Errorf("Lead returned unsupported output %T", item.Output.CustomizedOutput)
		}
		decision = &value
	}
	if decision == nil {
		return fmt.Errorf("Lead turn returned no decision")
	}
	finalized, err := r.applyDecision(ctx, *decision)
	if err != nil {
		return err
	}
	if finalized {
		turn.Loop.Stop()
	}
	return nil
}

func (r *sessionRuntime) decideLead(ctx context.Context, signals []Signal) (LeadDecision, error) {
	state, err := r.projection(ctx)
	if err != nil {
		return LeadDecision{}, err
	}
	lead, ok := r.profile.Lead(state.LeadID)
	if !ok {
		return LeadDecision{}, fmt.Errorf("selected Lead %s is absent from pinned profile", state.LeadID)
	}
	turn := state.LeadTurns + 1
	kinds := make([]string, 0, len(signals))
	for _, signal := range signals {
		kinds = append(kinds, signal.Kind)
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.LeadTurnStarted, Actor: lead.ID,
		Data: event.LeadTurnData{Turn: turn, SignalKinds: kinds},
	}); err != nil {
		return LeadDecision{}, err
	}
	system, user := leadMessages(r.profile, lead, state, signals)
	decision, err := r.generateLeadDecisionWithTools(ctx, lead, turn, system, user, state.WorkCount() == 0, func(decision LeadDecision) error {
		if strings.TrimSpace(decision.Assessment) == "" {
			return fmt.Errorf("assessment is empty")
		}
		if !decision.Finalize && len(decision.Delegations) == 0 && state.WorkCount() == 0 {
			return fmt.Errorf("no delegation or final answer was produced while no work was active")
		}
		if decision.Finalize && strings.TrimSpace(decision.FinalBrief) == "" {
			return fmt.Errorf("finalize is true but final_brief is empty")
		}
		return nil
	})
	if err != nil {
		return LeadDecision{}, fmt.Errorf("Lead turn %d: %w", turn, err)
	}
	decision.observedWork = state.WorkCount()
	return decision, nil
}

func (r *sessionRuntime) applyDecision(ctx context.Context, decision LeadDecision) (bool, error) {
	state, err := r.projection(ctx)
	if err != nil {
		return false, err
	}
	lead, _ := r.profile.Lead(state.LeadID)

	for _, id := range uniqueStrings(decision.Cancel) {
		if err := r.cancelDelegation(ctx, id, "cancelled by Lead coordination decision"); err != nil {
			return false, err
		}
	}

	specs, err := r.materializeDelegations(state, lead, decision.Delegations)
	if err != nil {
		return false, err
	}
	if !decision.Finalize && len(specs) == 0 && decision.observedWork == 0 {
		current, err := r.projection(ctx)
		if err != nil {
			return false, err
		}
		if current.WorkCount() == 0 {
			return false, fmt.Errorf(
				"Lead produced no delegation or final answer while no work was active",
			)
		}
	}
	_, err = r.store.Append(ctx, r.session.ID, event.Draft{
		Kind:  event.LeadDecision,
		Actor: lead.ID,
		Data: event.LeadDecisionData{
			Turn:          state.LeadTurns,
			Assessment:    decision.Assessment,
			Delegations:   specs,
			Cancellations: uniqueStrings(decision.Cancel),
			WillFinalize:  decision.Finalize,
		},
	})
	if err != nil {
		return false, err
	}
	for _, spec := range specs {
		if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.DelegationCreated, Actor: lead.ID,
			CorrelationID: spec.ID, Data: spec,
		}); err != nil {
			return false, err
		}
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.LeadTurnCompleted, Actor: lead.ID,
		Data: event.LeadTurnData{Turn: state.LeadTurns, Assessment: decision.Assessment},
	}); err != nil {
		return false, err
	}

	if decision.Finalize {
		r.cancelActiveDelegations(ctx, "Lead finalized with sufficient evidence")
		if err := r.generateFinal(ctx, lead, decision.FinalBrief); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := r.scheduleReady(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (r *sessionRuntime) materializeDelegations(
	state *Projection,
	lead profile.LeadAssignment,
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
		member, ok := r.profile.CallableMemberFor(lead, proposal.MemberID)
		if !ok {
			return nil, fmt.Errorf("Lead %s cannot access member %s", lead.ID, proposal.MemberID)
		}
		if strings.TrimSpace(proposal.Objective) == "" {
			return nil, fmt.Errorf("delegation %s has an empty objective", proposal.Key)
		}
		requiredTools := uniqueStrings(proposal.RequiredTools)
		permittedTools := make(map[string]struct{}, len(tooling.Supported()))
		for _, name := range tooling.Supported() {
			permittedTools[name] = struct{}{}
		}
		for _, name := range requiredTools {
			if _, permitted := permittedTools[name]; !permitted {
				return nil, fmt.Errorf(
					"delegation %s requires unknown runtime tool %s for member %s",
					proposal.Key, name, member.ID,
				)
			}
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
			ID:            id.String(),
			Key:           proposal.Key,
			MemberID:      proposal.MemberID,
			Objective:     strings.TrimSpace(proposal.Objective),
			Context:       strings.TrimSpace(proposal.Context),
			DependsOn:     dependencies,
			RequiredTools: requiredTools,
			Depth:         depth,
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
	lead, ok := r.profile.Lead(state.LeadID)
	if !ok {
		r.stopWithError(fmt.Errorf("selected Lead %s is absent", state.LeadID))
		return
	}
	member, ok := r.profile.CallableMemberFor(lead, delegation.Spec.MemberID)
	if !ok {
		r.stopWithError(fmt.Errorf("member %s is not permitted", delegation.Spec.MemberID))
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
		Kind: event.DelegationStarted, Actor: member.ID,
		CorrelationID: delegationID,
		Data:          event.DelegationStartedData{DelegationID: delegationID, Attempt: attempt},
	}); err != nil {
		r.stopWithError(err)
		return
	}
	var raw strings.Builder
	rawResult, runErr := r.runToolMember(ctx, member, delegation, attempt)
	raw.WriteString(rawResult)
	if runErr != nil {
		r.handleDelegationFailure(parent, delegationID, member.ID, attempt, runErr)
		return
	}
	result, parseErr := decodeJSONObject[MemberResult](raw.String())
	if parseErr != nil {
		result = MemberResult{Result: strings.TrimSpace(raw.String())}
	}
	if strings.TrimSpace(result.Result) == "" {
		r.handleDelegationFailure(parent, delegationID, member.ID, attempt, fmt.Errorf("member returned an empty result"))
		return
	}
	completed, err := r.store.Append(parent, r.session.ID, event.Draft{
		Kind: event.DelegationCompleted, Actor: member.ID,
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

func (r *sessionRuntime) generateFinal(ctx context.Context, lead profile.LeadAssignment, brief string) error {
	r.run.finalizing.Store(true)
	state, err := r.projection(ctx)
	if err != nil {
		return err
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalStarted, Actor: lead.ID,
	}); err != nil {
		return err
	}
	chat, modelSpec, err := r.providers.Model(ctx, r.profile, lead.Model, provider.Text)
	if err != nil {
		return err
	}
	chat = r.observeModel(lead.Model, "final:"+lead.ID)(chat)
	system, user := finalMessages(r.profile, lead, state, brief)
	handlers, err := r.tools.Handlers(ctx, "final:"+lead.ID, nil)
	if err != nil {
		return err
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "alt-final", Description: "Lead final synthesis",
		Instruction:   system + "\nUse inherited runtime tools only if verification is still necessary. Never narrate a future tool call; call it or return the complete answer.",
		Model:         chat,
		Handlers:      tooling.AgentHandlers(handlers...),
		MaxIterations: unboundedAgentIterations(),
	})
	if err != nil {
		return fmt.Errorf("create Eino final Lead agent: %w", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent, EnableStreaming: true, CheckPointStore: r.store,
	})
	iterator := runner.Run(
		ctx,
		[]*schema.Message{schema.UserMessage(user)},
		adk.WithCheckPointID("final:"+r.session.ID),
	)
	var answer string
	reasoningBuffer := newEventTextBuffer(1024, func(text string) error {
		_, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.FinalReasoning, Actor: lead.ID,
			Data: event.TextDeltaData{Text: text},
		})
		return err
	})
	for {
		item, ok := iterator.Next()
		if !ok {
			break
		}
		if item.Err != nil {
			return item.Err
		}
		if item.Output == nil || item.Output.MessageOutput == nil {
			continue
		}
		variant := item.Output.MessageOutput
		message, err := r.consumeAgentMessage(ctx, variant, "", lead.ID, false)
		if err != nil {
			return err
		}
		if message == nil {
			continue
		}
		switch variant.Role {
		case schema.Assistant:
			for _, call := range message.ToolCalls {
				if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
					Kind: event.ToolCalled, Actor: lead.ID,
					Data: event.ToolCallData{
						ToolCallID: call.ID, Tool: call.Function.Name,
						Arguments: call.Function.Arguments,
					},
				}); err != nil {
					return err
				}
			}
			if text := strings.TrimSpace(messageText(message)); text != "" && len(message.ToolCalls) == 0 {
				answer = text
			}
			if reasoning := messageReasoning(message); reasoning != "" && r.profile.Policy.PersistReasoning {
				if err := reasoningBuffer.Add(reasoning); err != nil {
					return err
				}
			}
			if err := recordUsage(
				ctx, r.store, r.session.ID, lead.Model,
				"final:"+lead.ID, modelSpec, message.ResponseMeta,
			); err != nil {
				return err
			}
		case schema.Tool:
			toolError, failed := tooling.ParseRecoverableToolError(messageText(message))
			if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
				Kind: event.ToolCompleted, Actor: lead.ID,
				Data: event.ToolCompletedData{
					ToolCallID: message.ToolCallID,
					Tool:       firstNonEmpty(message.ToolName, variant.ToolName),
					Failed:     failed,
					Error:      toolError,
					Result:     messageText(message),
				},
			}); err != nil {
				return err
			}
		}
	}
	if err := reasoningBuffer.Flush(); err != nil {
		return err
	}
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("Lead returned an empty final answer")
	}
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalTextDelta, Actor: lead.ID,
		Data: event.TextDeltaData{Text: answer},
	}); err != nil {
		return err
	}
	_, err = r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.FinalCompleted, Actor: lead.ID,
		Data: event.FinalCompletedData{Answer: answer},
	})
	return err
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
		Kind: event.DelegationCancelled, Actor: state.LeadID,
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
