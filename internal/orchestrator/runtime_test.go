package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestCurrentRuntimeUsesCancellationWithoutInventedCeilings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		t.Fatal("current profile received an ALT-authored run deadline")
	}
	if got, want := unboundedAgentIterations(), int(^uint(0)>>1); got != want {
		t.Fatalf("agent iteration sentinel = %d, want platform max %d", got, want)
	}

	actualFailure := &Projection{Delegations: map[string]*Delegation{
		"failed": {
			Status: DelegationFailed, Attempt: 1,
		},
	}}
	if got := actualFailure.WorkCount(); got != 0 {
		t.Fatalf("current model failure counted as automatically retryable work: %d", got)
	}
	interrupted := &Projection{Delegations: map[string]*Delegation{
		"interrupted": {
			Status: DelegationFailed, Attempt: 1, Interrupted: true,
		},
	}}
	if got := interrupted.WorkCount(); got != 1 {
		t.Fatalf("interrupted durable work count = %d, want 1 recovery attempt", got)
	}
}

func TestAdaptiveParallelAndSequentialLanes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	factory := newScenarioFactory()
	registry := provider.NewRegistry()
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := profile.Validate(document.Profile); profile.HasErrors(diagnostics) {
		t.Fatalf("test profile invalid: %#v", diagnostics)
	}

	run, err := NewEngine(ledger, registry).Start(ctx, document, "Review this durable concurrent Go architecture.")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-factory.goStarted:
		events, err := ledger.Events(ctx, run.SessionID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if hasKindActor(events, event.DelegationCompleted, "persistence") {
			t.Fatal("persistence completed before its release gate")
		}
	case <-ctx.Done():
		t.Fatal("Go follow-up did not start while persistence remained active")
	}
	close(factory.releasePersistence)

	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}
	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != store.SessionCompleted {
		t.Fatalf("session status = %s", session.Status)
	}
	if session.FinalAnswer != "One accountable final answer." {
		t.Fatalf("unexpected final answer %q", session.FinalAnswer)
	}

	events, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	securityDone := sequenceOf(t, events, event.DelegationCompleted, "security")
	goStarted := sequenceOf(t, events, event.DelegationStarted, "go-runtime")
	persistenceDone := sequenceOf(t, events, event.DelegationCompleted, "persistence")
	researchStarted := sequenceOf(t, events, event.DelegationStarted, "research")
	if !(securityDone < goStarted && goStarted < persistenceDone && persistenceDone < researchStarted) {
		t.Fatalf(
			"unexpected adaptive ordering: security done=%d, Go started=%d, persistence done=%d, research started=%d",
			securityDone, goStarted, persistenceDone, researchStarted,
		)
	}
}

func TestPeerCollaborationIsStatefulScopedAndLeadAccountable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	factory := &peerScenarioFactory{}
	registry := provider.NewRegistry()
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewEngine(ledger, registry).Start(ctx, document, "Reconcile a difficult recovery design with a research peer.")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}

	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.FinalAnswer != "The Lead's accountable synthesis." {
		t.Fatalf("final answer = %q", session.FinalAnswer)
	}
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var created []event.PeerTurnSpec
	for _, item := range items {
		if item.Kind != event.PeerTurnCreated {
			continue
		}
		spec, decodeErr := event.Decode[event.PeerTurnSpec](item)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		created = append(created, spec)
	}
	if len(created) != 2 {
		logEvents(t, ledger, run.SessionID)
		t.Fatalf("peer turns = %d, want 2", len(created))
	}
	if created[0].CollaborationID == "" || created[0].CollaborationID != created[1].CollaborationID {
		t.Fatalf("collaboration ids = %q, %q", created[0].CollaborationID, created[1].CollaborationID)
	}
	if created[0].Round != 1 || created[1].Round != 2 {
		t.Fatalf("rounds = %d, %d", created[0].Round, created[1].Round)
	}
	if !hasKindActor(items, event.PeerTurnCompleted, "research") {
		t.Fatal("peer completion was not durably recorded")
	}
	if hasKindActor(items, event.FinalCompleted, "research") {
		t.Fatal("peer was allowed to complete the user answer")
	}
	if !hasKindActor(items, event.FinalCompleted, "architecture-lead") {
		t.Fatal("accountable Lead final completion missing")
	}

	prompts := factory.promptsSnapshot()
	if len(prompts) != 2 {
		t.Fatalf("peer prompts = %d, want 2", len(prompts))
	}
	if strings.Contains(prompts[0], "first-round finding") {
		t.Fatal("first peer round received invented prior collaboration history")
	}
	if !strings.Contains(prompts[1], "first-round finding") || !strings.Contains(prompts[1], `"current_round": 2`) {
		t.Fatalf("second peer round did not receive scoped first-round history:\n%s", prompts[1])
	}
}

func TestIdleLeadDecisionRequiresExplicitCorrectionBeforeFinalization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	registry := provider.NewRegistry()
	if err := registry.Register(idleDecisionFactory{}); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}

	run, err := NewEngine(ledger, registry).Start(ctx, document, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}

	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != store.SessionCompleted {
		t.Fatalf("session status = %s, want %s", session.Status, store.SessionCompleted)
	}
	if session.FinalAnswer != "Hello!" {
		t.Fatalf("final answer = %q, want %q", session.FinalAnswer, "Hello!")
	}

	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var plan event.LeadDecisionData
	var found bool
	for _, item := range items {
		if item.Kind != event.LeadDecision {
			continue
		}
		plan, err = event.Decode[event.LeadDecisionData](item)
		if err != nil {
			t.Fatal(err)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("lead.decision event missing")
	}
	if !plan.WillFinalize {
		t.Fatal("corrected Lead decision did not explicitly finalize")
	}
	leadCalls := 0
	for _, item := range items {
		if item.Kind == event.ModelCallStarted && item.Actor == "lead:architecture-lead" {
			leadCalls++
		}
	}
	if leadCalls != 2 {
		t.Fatalf("Lead model calls = %d, want initial plus one explicit correction", leadCalls)
	}
	if !hasKindActor(items, event.FinalCompleted, "architecture-lead") {
		t.Fatal("final completion event missing")
	}
}

func TestUserInstructionIsPersistedAndPushedIntoEinoTurnLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	factory := newScenarioFactory()
	registry := provider.NewRegistry()
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(ledger, registry)
	run, err := engine.Start(ctx, document, "Review this durable concurrent Go architecture.")
	if err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, ledger, run.SessionID, event.DelegationStarted, "persistence", 5*time.Second)

	const instruction = "Prioritize cancellation behavior in the final answer."
	if err := engine.Steer(ctx, run.SessionID, instruction); err != nil {
		t.Fatal(err)
	}
	waitForSignalKind(t, ledger, run.SessionID, string(event.UserInstruction), 5*time.Second)

	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var persisted bool
	for _, item := range items {
		if item.Kind != event.UserInstruction {
			continue
		}
		data, decodeErr := event.Decode[event.UserInstructionData](item)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		persisted = data.Text == instruction
	}
	if !persisted {
		t.Fatal("user instruction was not durably recorded")
	}

	close(factory.releasePersistence)
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}
}

func TestInterruptedDelegationIsRetriedOnRecovery(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	factory := newScenarioFactory()
	registry := provider.NewRegistry()
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}

	firstCtx, stopFirst := context.WithCancel(ctx)
	firstEngine := NewEngine(ledger, registry)
	run, err := firstEngine.Start(firstCtx, document, "Review this durable concurrent Go architecture.")
	if err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, ledger, run.SessionID, event.DelegationStarted, "persistence", 5*time.Second)
	stopFirst()
	if err := run.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run error = %v, want context canceled", err)
	}

	close(factory.releasePersistence)
	secondEngine := NewEngine(ledger, registry)
	resumed, err := secondEngine.Resume(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := resumed.Wait(waitCtx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}

	events, err := ledger.Events(context.Background(), run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	completions := 0
	for _, item := range events {
		if item.Actor != "persistence" {
			continue
		}
		if item.Kind == event.DelegationStarted {
			starts++
		}
		if item.Kind == event.DelegationCompleted {
			completions++
		}
	}
	if starts != 2 {
		logEvents(t, ledger, run.SessionID)
		t.Fatalf("persistence starts = %d, want 2 attempts", starts)
	}
	if completions != 1 {
		t.Fatalf("persistence completions = %d, want exactly 1", completions)
	}
}

func TestMemberUsesEinoFilesystemToolLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("durable evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	registry := provider.NewRegistry()
	if err := registry.Register(toolScenarioFactory{}); err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(toolTestProfile))
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewEngine(ledger, registry).StartAt(
		ctx, document, "Read the workspace evidence and report it.", workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKindActor(items, event.ToolCalled, "reader") ||
		!hasKindActor(items, event.ToolCompleted, "reader") {
		t.Fatalf("tool lifecycle events missing")
	}
	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.FinalAnswer != "Evidence was verified." {
		t.Fatalf("final answer = %q", session.FinalAnswer)
	}
}

type idleDecisionFactory struct{}

func (idleDecisionFactory) Descriptor() provider.GatewayDescriptor {
	return testGatewayDescriptor()
}
func (idleDecisionFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return testCatalog(), nil
}

func (idleDecisionFactory) NewChatModel(
	_ context.Context,
	spec profile.Model,
	_ provider.Mode,
) (model.BaseChatModel, error) {
	return &idleDecisionModel{name: spec.Name}, nil
}

type idleDecisionModel struct {
	name string
}

func (m *idleDecisionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *idleDecisionModel) Generate(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	switch m.name {
	case "router":
		return response(`{"lead_id":"architecture-lead","confidence":1,"basis":"Use the accountable Lead for the greeting."}`), nil
	case "lead":
		for _, message := range input {
			if strings.Contains(message.Content, "cannot advance the session") {
				return response(`{"assessment":"No delegated evidence is required.","delegations":[],"cancel":[],"finalize":true,"final_brief":"Answer the greeting directly."}`), nil
			}
		}
		for _, message := range input {
			if strings.Contains(message.Content, "CURRENT SESSION STATE:") {
				return response(`{"assessment":"The greeting needs no delegated work.","delegations":[],"cancel":[],"finalize":false,"final_brief":""}`), nil
			}
		}
		return response("Hello!"), nil
	default:
		return nil, fmt.Errorf("unsupported Generate model %s", m.name)
	}
}

func (m *idleDecisionModel) Stream(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(context.Background(), input)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

type toolScenarioFactory struct{}

func (toolScenarioFactory) Descriptor() provider.GatewayDescriptor {
	return testGatewayDescriptor()
}
func (toolScenarioFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return testCatalog(), nil
}

func (toolScenarioFactory) NewChatModel(
	_ context.Context,
	spec profile.Model,
	_ provider.Mode,
) (model.BaseChatModel, error) {
	if spec.Name == "tool-member" {
		return &toolScenarioModel{name: spec.Name}, nil
	}
	return &toolScenarioModel{name: spec.Name}, nil
}

type toolScenarioModel struct {
	name string
}

func (m *toolScenarioModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *toolScenarioModel) Generate(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	switch m.name {
	case "router":
		return response(`{"lead_id":"implementation-lead","confidence":1,"basis":"The task requires direct workspace evidence."}`), nil
	case "lead":
		state := parseLeadState(input[len(input)-1].Content)
		if len(state.Delegations) == 0 {
			return response(`{"assessment":"Have the reader inspect the evidence.","delegations":[{"key":"read","member_id":"reader","objective":"Read evidence.txt and report its content.","context":"","depends_on":[]}],"cancel":[],"finalize":false,"final_brief":""}`), nil
		}
		return response(`{"assessment":"The requested workspace evidence is available.","delegations":[],"cancel":[],"finalize":true,"final_brief":"Report the verified evidence."}`), nil
	default:
		return nil, fmt.Errorf("unsupported Generate model %s", m.name)
	}
}

func (m *toolScenarioModel) Stream(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	switch m.name {
	case "tool-member":
		for _, message := range input {
			if message.Role == schema.Tool {
				return schema.StreamReaderFromArray([]*schema.Message{response(
					`{"result":"durable evidence","findings":["workspace file read"],"risks":[],"confidence":1}`,
				)}), nil
			}
		}
		return schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID: "read-1", Type: "function",
				Function: schema.FunctionCall{
					Name: "read_file", Arguments: `{"file_path":"evidence.txt","offset":0,"limit":20}`,
				},
			}},
		}}), nil
	case "lead":
		for _, message := range input {
			if strings.Contains(message.Content, "CURRENT SESSION STATE:") {
				state := parseLeadState(message.Content)
				decision := `{"assessment":"The requested workspace evidence is available.","delegations":[],"cancel":[],"finalize":true,"final_brief":"Report the verified evidence."}`
				if len(state.Delegations) == 0 {
					decision = `{"assessment":"Have the reader inspect the evidence.","delegations":[{"key":"read","member_id":"reader","objective":"Read evidence.txt and report its content.","context":"","depends_on":[]}],"cancel":[],"finalize":false,"final_brief":""}`
				}
				return schema.StreamReaderFromArray([]*schema.Message{response(decision)}), nil
			}
			if message.Role == schema.Tool {
				return schema.StreamReaderFromArray([]*schema.Message{
					response("Evidence was verified."),
				}), nil
			}
		}
		return schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant, Content: "I will inspect the workspace first.",
			ToolCalls: []schema.ToolCall{{
				ID: "final-ls", Type: "function",
				Function: schema.FunctionCall{
					Name: "ls", Arguments: `{"path":".","depth":1}`,
				},
			}},
		}}), nil
	default:
		return nil, fmt.Errorf("unsupported Stream model %s", m.name)
	}
}

type scenarioFactory struct {
	releasePersistence chan struct{}
	goStarted          chan struct{}
	goStartedOnce      sync.Once
}

type peerScenarioFactory struct {
	mu      sync.Mutex
	prompts []string
}

func (*peerScenarioFactory) Descriptor() provider.GatewayDescriptor { return testGatewayDescriptor() }
func (*peerScenarioFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return testCatalog(), nil
}
func (f *peerScenarioFactory) NewChatModel(_ context.Context, spec profile.Model, mode provider.Mode) (model.BaseChatModel, error) {
	return &peerScenarioModel{factory: f, name: spec.Name, mode: mode}, nil
}
func (f *peerScenarioFactory) promptsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

type peerScenarioModel struct {
	factory *peerScenarioFactory
	name    string
	mode    provider.Mode
}

func (m *peerScenarioModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}
func (m *peerScenarioModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	switch m.name {
	case "router":
		return response(`{"lead_id":"architecture-lead","confidence":0.99,"basis":"The architecture Lead owns the requested recovery synthesis."}`), nil
	case "lead":
		state := parseLeadState(input[len(input)-1].Content)
		if len(state.PeerTurns) == 0 {
			return response(`{"assessment":"Begin a focused collaboration.","delegations":[],"peer_turns":[{"key":"research-1","peer_id":"research","collaboration_id":"","objective":"Establish the recovery invariant.","context":""}],"cancel":[],"finalize":false,"final_brief":""}`), nil
		}
		if len(state.PeerTurns) == 1 && state.PeerTurns[0].Status == string(DelegationCompleted) {
			return response(`{"assessment":"Refine the first finding in the same collaboration.","delegations":[],"peer_turns":[{"key":"research-2","peer_id":"research","collaboration_id":"` + state.PeerTurns[0].CollaborationID + `","objective":"Stress-test the invariant using the first-round result.","context":"Focus on crash boundaries."}],"cancel":[],"finalize":false,"final_brief":""}`), nil
		}
		if len(state.PeerTurns) == 2 && state.PeerTurns[1].Status == string(DelegationCompleted) {
			return response(`{"assessment":"The iterative peer work is sufficient.","delegations":[],"peer_turns":[],"cancel":[],"finalize":true,"final_brief":"Synthesize both rounds."}`), nil
		}
		return response(`{"assessment":"Wait for the active peer round.","delegations":[],"peer_turns":[],"cancel":[],"finalize":false,"final_brief":""}`), nil
	default:
		return nil, errors.New("Generate is unsupported for " + m.name)
	}
}
func (m *peerScenarioModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	switch m.name {
	case "research":
		prompt := input[len(input)-1].Content
		m.factory.mu.Lock()
		m.factory.prompts = append(m.factory.prompts, prompt)
		round := len(m.factory.prompts)
		m.factory.mu.Unlock()
		result := `{"result":"first-round finding","findings":["invariant"],"risks":[],"confidence":0.8}`
		if round == 2 {
			result = `{"result":"second-round refinement","findings":["crash boundary"],"risks":[],"confidence":0.9}`
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: result}}), nil
	case "lead":
		for _, message := range input {
			if strings.Contains(message.Content, "CURRENT SESSION STATE:") {
				decision, err := m.Generate(context.Background(), []*schema.Message{message})
				if err != nil {
					return nil, err
				}
				return schema.StreamReaderFromArray([]*schema.Message{decision}), nil
			}
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "The Lead's accountable synthesis."}}), nil
	default:
		return nil, errors.New("Stream is unsupported for " + m.name)
	}
}

func (*scenarioFactory) Descriptor() provider.GatewayDescriptor {
	return testGatewayDescriptor()
}

func testGatewayDescriptor() provider.GatewayDescriptor {
	return provider.GatewayDescriptor{
		ID: "opencode", Name: "Test Gateway",
		CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
		Routes:                []provider.GatewayRoute{{ID: "test", Label: "Test"}},
		MultiModelCatalog:     true,
	}
}
func (*scenarioFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return testCatalog(), nil
}

func testCatalog() []provider.CatalogModel {
	names := []string{
		"router", "lead", "alternate", "security", "persistence",
		"go", "research", "tool-member",
	}
	result := make([]provider.CatalogModel, 0, len(names))
	for _, name := range names {
		result = append(result, provider.CatalogModel{
			Gateway: "opencode", Route: "test", ID: name,
			Capabilities: provider.Capabilities{
				StructuredOutput: provider.CapabilityUnknown,
				ToolCalling:      provider.CapabilitySupported,
			},
		})
	}
	return result
}

func newScenarioFactory() *scenarioFactory {
	return &scenarioFactory{
		releasePersistence: make(chan struct{}),
		goStarted:          make(chan struct{}),
	}
}

func (f *scenarioFactory) NewChatModel(_ context.Context, spec profile.Model, mode provider.Mode) (model.BaseChatModel, error) {
	return &scenarioModel{factory: f, name: spec.Name, mode: mode}, nil
}

type scenarioModel struct {
	factory *scenarioFactory
	name    string
	mode    provider.Mode
}

func (m *scenarioModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scenarioModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	switch m.name {
	case "router":
		return response(`{"lead_id":"architecture-lead","confidence":0.99,"basis":"The task is dominated by cross-cutting concurrency and recovery architecture."}`), nil
	case "lead":
		state := parseLeadState(input[len(input)-1].Content)
		return response(m.leadDecision(state)), nil
	default:
		return nil, errors.New("Generate is unsupported for " + m.name)
	}
}

func (m *scenarioModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](4)
	go func() {
		defer writer.Close()
		switch m.name {
		case "security":
			sendChunks(writer, `{"result":"security complete","findings":["auth boundary"],"risks":[],"confidence":0.9}`)
		case "persistence":
			select {
			case <-ctx.Done():
				writer.Send(nil, ctx.Err())
				return
			case <-m.factory.releasePersistence:
			}
			sendChunks(writer, `{"result":"persistence complete","findings":["WAL ordering"],"risks":[],"confidence":0.9}`)
		case "go":
			m.factory.goStartedOnce.Do(func() { close(m.factory.goStarted) })
			sendChunks(writer, `{"result":"Go follow-up complete","findings":["cancellation"],"risks":[],"confidence":0.9}`)
		case "research":
			sendChunks(writer, `{"result":"recovery follow-up complete","findings":["resume invariant"],"risks":[],"confidence":0.9}`)
		case "lead":
			for _, message := range input {
				if strings.Contains(message.Content, "CURRENT SESSION STATE:") {
					sendChunks(writer, m.leadDecision(parseLeadState(message.Content)))
					return
				}
			}
			sendChunks(writer, "One accountable final answer.")
		default:
			writer.Send(nil, errors.New("Stream is unsupported for "+m.name))
		}
	}()
	return reader, nil
}

func (m *scenarioModel) leadDecision(state leadStateView) string {
	if len(state.Delegations) == 0 {
		return `{"assessment":"Start independent security and persistence reviews.","delegations":[{"key":"security","member_id":"security","objective":"Review security boundaries.","context":"","depends_on":[]},{"key":"persistence","member_id":"persistence","objective":"Review durable ordering.","context":"","depends_on":[]}],"cancel":[],"finalize":false,"final_brief":""}`
	}
	security := state.byMember("security")
	goRuntime := state.byMember("go-runtime")
	persistence := state.byMember("persistence")
	research := state.byMember("research")
	if security.Status == string(DelegationCompleted) && goRuntime.ID == "" {
		return `{"assessment":"Use the security result to formulate a richer Go runtime review while persistence continues.","delegations":[{"key":"go","member_id":"go-runtime","objective":"Evaluate cancellation using the completed security finding.","context":"","depends_on":["` + security.ID + `"]}],"cancel":[],"finalize":false,"final_brief":""}`
	}
	if goRuntime.Status == string(DelegationCompleted) && persistence.Status != string(DelegationCompleted) {
		return `{"assessment":"The security lineage is complete; wait for the independent persistence branch.","delegations":[],"cancel":[],"finalize":false,"final_brief":""}`
	}
	if persistence.Status == string(DelegationCompleted) && research.ID == "" {
		return `{"assessment":"Use the persistence result to formulate a recovery-specific follow-up.","delegations":[{"key":"research","member_id":"research","objective":"Validate the recovery invariant exposed by persistence.","context":"","depends_on":["` + persistence.ID + `"]}],"cancel":[],"finalize":false,"final_brief":""}`
	}
	if research.Status == string(DelegationCompleted) {
		return `{"assessment":"All required evidence is available for synthesis.","delegations":[],"cancel":[],"finalize":true,"final_brief":"Reconcile all completed branches."}`
	}
	return `{"assessment":"Useful delegated work remains active.","delegations":[],"cancel":[],"finalize":false,"final_brief":""}`
}

type leadStateView struct {
	Delegations []struct {
		ID       string `json:"id"`
		MemberID string `json:"member_id"`
		Status   string `json:"status"`
	} `json:"delegations"`
	PeerTurns []struct {
		ID              string `json:"id"`
		CollaborationID string `json:"collaboration_id"`
		PeerID          string `json:"peer_id"`
		Round           int    `json:"round"`
		Status          string `json:"status"`
	} `json:"peer_turns"`
}

func (s leadStateView) byMember(id string) struct {
	ID       string `json:"id"`
	MemberID string `json:"member_id"`
	Status   string `json:"status"`
} {
	for _, delegation := range s.Delegations {
		if delegation.MemberID == id {
			return delegation
		}
	}
	return struct {
		ID       string `json:"id"`
		MemberID string `json:"member_id"`
		Status   string `json:"status"`
	}{}
}

func parseLeadState(prompt string) leadStateView {
	const marker = "CURRENT SESSION STATE:\n"
	index := strings.Index(prompt, marker)
	if index < 0 {
		panic("Lead state marker missing")
	}
	var state leadStateView
	if err := json.Unmarshal([]byte(prompt[index+len(marker):]), &state); err != nil {
		panic(err)
	}
	return state
}

func response(content string) *schema.Message {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: content,
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20,
		}},
	}
}

func sendChunks(writer *schema.StreamWriter[*schema.Message], content string) {
	middle := len(content) / 2
	writer.Send(&schema.Message{Role: schema.Assistant, Content: content[:middle]}, nil)
	writer.Send(&schema.Message{
		Role: schema.Assistant, Content: content[middle:],
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20,
		}},
	}, nil)
}

func hasKindActor(events []event.Event, kind event.Kind, actor string) bool {
	for _, item := range events {
		if item.Kind == kind && item.Actor == actor {
			return true
		}
	}
	return false
}

func sequenceOf(t *testing.T, events []event.Event, kind event.Kind, actor string) int64 {
	t.Helper()
	for _, item := range events {
		if item.Kind == kind && item.Actor == actor {
			return item.Sequence
		}
	}
	t.Fatalf("missing %s event for %s", kind, actor)
	return 0
}

func waitForEvent(t *testing.T, ledger *store.Store, sessionID string, kind event.Kind, actor string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := ledger.Events(context.Background(), sessionID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if hasKindActor(events, kind, actor) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s event for %s", kind, actor)
}

func waitForSignalKind(t *testing.T, ledger *store.Store, sessionID, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := ledger.Events(context.Background(), sessionID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range events {
			if item.Kind != event.LeadTurnStarted {
				continue
			}
			data, decodeErr := event.Decode[event.LeadTurnData](item)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			for _, signalKind := range data.SignalKinds {
				if signalKind == kind {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Lead turn signal %s", kind)
}

func logEvents(t *testing.T, ledger *store.Store, sessionID string) {
	t.Helper()
	events, err := ledger.Events(context.Background(), sessionID, 0)
	if err != nil {
		t.Log(err)
		return
	}
	for _, item := range events {
		t.Logf("%03d %-30s %-14s %s", item.Sequence, item.Kind, item.Actor, item.Data)
	}
}

const testProfile = `
schema: 1
id: test-team
revision: 1
name: Test Team
gateway: opencode
models:
  router-model: {route: test, name: router}
  lead-model: {route: test, name: lead}
  alternate-model: {route: test, name: alternate}
  security-model: {route: test, name: security}
  persistence-model: {route: test, name: persistence}
  go-model: {route: test, name: go}
  research-model: {route: test, name: research}
router:
  model: router-model
  definition: Select the Lead whose explicit ownership covers the requested result.
leads:
  - id: architecture-lead
    model: lead-model
    definition: Own cross-cutting architecture decisions involving concurrency, durable recovery, authority, and implementation domains.
    calls: [security, persistence, go-runtime, research]
    peers: [research]
  - id: alternate-lead
    model: alternate-model
    definition: Own unrelated alternative requests so the Team has a genuine routing choice.
members:
  - {id: security, model: security-model, definition: Review security boundaries and authority enforcement in the design.}
  - {id: persistence, model: persistence-model, definition: Review durable ordering and recovery behavior in the design.}
  - {id: go-runtime, model: go-model, definition: Review Go concurrency and cancellation informed by earlier evidence.}
  - {id: research, model: research-model, definition: Validate a narrow recovery invariant exposed by earlier evidence.}
`

const toolTestProfile = `
schema: 1
id: tool-team
revision: 1
name: Tool Team
gateway: opencode
models:
  router-model: {route: test, name: router}
  lead-model: {route: test, name: lead}
  alternate-model: {route: test, name: alternate}
  member-model: {route: test, name: tool-member}
router:
  model: router-model
  definition: Select the Lead whose ownership matches the requested result.
leads:
  - id: implementation-lead
    model: lead-model
    definition: Own implementation questions requiring direct codebase inspection and accountable synthesis of verified evidence.
    calls: [reader]
  - id: alternate-lead
    model: alternate-model
    definition: Own unrelated requests so the Team has a genuine routing choice.
members:
  - id: reader
    model: member-model
    definition: Inspect files in the session workspace and return precise evidence.
`
