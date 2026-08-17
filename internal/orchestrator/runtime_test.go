package orchestrator

import (
	"context"
	"fmt"
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
		t.Fatal("ALT authored a run deadline")
	}
	if got, want := unboundedAgentIterations(), int(^uint(0)>>1); got != want {
		t.Fatalf("agent iteration sentinel = %d, want platform max %d", got, want)
	}
}

func TestSimpleRequestIsOnePrimaryModelCall(t *testing.T) {
	gateway := newScriptedGateway(func(name string, _ int, _ []*schema.Message) string {
		if name != "deepseek-code" {
			t.Fatalf("unexpected model %s", name)
		}
		return "The primary handled this request directly."
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, "Explain the parser invariant.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	if calls := gateway.callsSnapshot(); len(calls) != 1 || calls[0].Model != "deepseek-code" {
		t.Fatalf("model calls = %#v, want exactly one primary call", calls)
	}
	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.FinalAnswer != "The primary handled this request directly." || session.LeaderID != "deepseek-coder" {
		t.Fatalf("unexpected session projection: %#v", session)
	}
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind != event.AgentTurnCompleted || item.Actor != "deepseek-coder" {
			continue
		}
		completed, decodeErr := event.Decode[event.AgentTurnData](item)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if completed.Turn != 1 {
			t.Fatalf("completed agent turn = %d, want 1", completed.Turn)
		}
		return
	}
	t.Fatal("direct answer did not complete its numbered agent turn")
}

func TestInvalidHandoffIsRejectedBeforeDecisionIsPersisted(t *testing.T) {
	gateway := newScriptedGateway(func(name string, _ int, _ []*schema.Message) string {
		if name != "deepseek-code" {
			t.Fatalf("unexpected model %s", name)
		}
		return coordinateHandoff("unknown-peer", "Send it elsewhere.")
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, "Keep the ledger valid.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(ctx); err == nil || !strings.Contains(err.Error(), "cannot hand leadership") {
		t.Fatalf("invalid handoff error = %v", err)
	}
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == event.AgentDecision || item.Kind == event.AgentTurnCompleted || item.Kind == event.LeadershipTransferred && item.Actor != "system" {
			t.Fatalf("invalid handoff left a semantic transition in the ledger: %#v", item)
		}
	}
}

func TestHandoffPeerAnswersDirectlyAndNextTurnReentersPrimary(t *testing.T) {
	gateway := newScriptedGateway(func(name string, _ int, input []*schema.Message) string {
		user := exactUserText(input)
		switch {
		case name == "deepseek-code" && user == "FIRST EXACT REQUEST":
			return coordinateHandoff("research-peer", "The requested deliverable is an evidence audit.")
		case name == "research" && user == "FIRST EXACT REQUEST":
			return "The research peer answered the user directly."
		case name == "deepseek-code" && user == "SECOND EXACT REQUEST":
			return "The primary received the next user turn."
		default:
			t.Fatalf("unexpected call model=%s exact-user=%q", name, user)
			return ""
		}
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	first, err := engine.StartAt(ctx, document, "FIRST EXACT REQUEST", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, first)
	firstEvents, err := ledger.Events(ctx, first.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKindActor(firstEvents, event.FinalCompleted, "research-peer") {
		t.Fatal("handoff recipient did not directly complete the answer")
	}
	if hasKindActor(firstEvents, event.FinalCompleted, "deepseek-coder") {
		t.Fatal("primary relayed or synthesized the peer's answer")
	}

	second, err := engine.Continue(ctx, first.SessionID, "SECOND EXACT REQUEST")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, second)
	calls := gateway.callsSnapshot()
	want := []string{"deepseek-code", "research", "deepseek-code"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v", calls)
	}
	for index, expected := range want {
		if calls[index].Model != expected {
			t.Fatalf("call %d model = %s, want %s", index, calls[index].Model, expected)
		}
	}
	if !modelMessagesHavePrefix(calls[2].Messages, calls[0].Messages) {
		t.Fatalf("primary's next user turn did not extend its first model-visible request\nfirst=%s\nnext=%s", allMessageText(calls[0].Messages), allMessageText(calls[2].Messages))
	}
	if exactUserText(calls[2].Messages) != "SECOND EXACT REQUEST" {
		t.Fatal("primary did not receive the next exact user message word-for-word")
	}
}

func TestHandoffToolReturnsControlToALTWithoutPrimarySynthesis(t *testing.T) {
	gateway := newScriptedGateway(func(string, int, []*schema.Message) string {
		t.Fatal("plain scripted response should not be used")
		return ""
	})
	gateway.respondMessage = func(name string, _ int, _ []*schema.Message) *schema.Message {
		switch name {
		case "deepseek-code":
			return toolCallResponse(toolNameHandoffLeadership, `{"peer_id":"research-peer","reason":"The requested result is the evidence audit."}`)
		case "research":
			return response("The peer tool recipient answered the user directly.")
		default:
			t.Fatalf("unexpected model %s", name)
			return nil
		}
	}
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, "Audit the evidence.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKindActor(items, event.LeadershipTransferred, "deepseek-coder") || !hasKindActor(items, event.FinalCompleted, "research-peer") {
		t.Fatal("return-direct handoff tool did not transfer authority to the peer")
	}
	for _, item := range items {
		if item.Kind != event.ToolCalled && item.Kind != event.ToolCompleted {
			continue
		}
		if strings.Contains(string(item.Data), toolNameHandoffLeadership) {
			t.Fatal("ALT control tool was misrepresented as runtime work")
		}
	}
}

func TestCoordinateToolCreatesStatelessSpecialistCall(t *testing.T) {
	gateway := newScriptedGateway(func(string, int, []*schema.Message) string {
		t.Fatal("plain scripted response should not be used")
		return ""
	})
	gateway.respondMessage = func(name string, ordinal int, _ []*schema.Message) *schema.Message {
		switch name {
		case "deepseek-code":
			if ordinal == 1 {
				return toolCallResponse(toolNameCoordinateTeam, `{"assessment":"The pixels contain required evidence.","delegations":[{"key":"read-diagnostic","specialist_id":"vision-specialist","objective":"Read the exact compiler diagnostic visible in the explicitly attached image.","context":"Return the filename, line, and complete message.","attachments":[],"depends_on":[]}],"peer_turns":[],"cancel":[]}`)
			}
			return response("The primary used the clean-slate visual result and answered.")
		case "vision":
			return response(`{"result":"parser.go:42 undefined symbol","findings":[],"risks":[],"confidence":1}`)
		default:
			t.Fatalf("unexpected model %s", name)
			return nil
		}
	}
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, "Fix the diagnostic in the screenshot.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	items, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKindActor(items, event.DelegationCreated, "deepseek-coder") || !hasKindActor(items, event.DelegationCompleted, "vision-specialist") || !hasKindActor(items, event.FinalCompleted, "deepseek-coder") {
		t.Fatal("return-direct coordination tool did not run the specialist and return to its caller")
	}
}

func TestHandoffRecipientCanHandLeadershipToAnotherPeer(t *testing.T) {
	p := visionCodingProfile(false)
	p.Models["architecture-model"] = profile.Model{Route: "test", Name: "architecture"}
	p.Peers = append(p.Peers, profile.AgentAssignment{
		ID: "architecture-peer", Model: "architecture-model",
		Definition: "Own cross-cutting architecture decisions once the evidence question has been narrowed.",
	})
	p.Peers[0].Peers = []string{"architecture-peer"}
	gateway := newScriptedGateway(func(name string, _ int, _ []*schema.Message) string {
		switch name {
		case "deepseek-code":
			return coordinateHandoff("research-peer", "Research must establish the controlling evidence.")
		case "research":
			return coordinateHandoff("architecture-peer", "The remaining deliverable is architectural.")
		case "architecture":
			return "The second peer now owns and answers the request."
		default:
			t.Fatalf("unexpected model %s", name)
			return ""
		}
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, p)
	run, err := engine.StartAt(ctx, document, "Trace the authority chain.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	events, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var transfers []event.LeadershipTransferredData
	for _, item := range events {
		if item.Kind == event.LeadershipTransferred {
			data, decodeErr := event.Decode[event.LeadershipTransferredData](item)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			transfers = append(transfers, data)
		}
	}
	if len(transfers) != 3 || transfers[1].FromAgentID != "deepseek-coder" || transfers[2].FromAgentID != "research-peer" {
		t.Fatalf("leadership chain = %#v", transfers)
	}
	if !hasKindActor(events, event.FinalCompleted, "architecture-peer") {
		t.Fatal("terminal peer did not answer directly")
	}
}

func TestPeerConsultationReturnsToCallerWithoutMovingLeadership(t *testing.T) {
	gateway := newScriptedGateway(func(name string, ordinal int, _ []*schema.Message) string {
		switch name {
		case "deepseek-code":
			if ordinal == 1 {
				return `{"kind":"coordinate","assessment":"Check the language specification before changing code.","delegations":[],"peer_turns":[{"key":"spec-check","peer_id":"research-peer","collaboration_id":"","objective":"Check whether the syntax is supported by the current language specification.","context":"Return only the controlling finding.","attachments":[]}],"cancel":[],"handoff":null}`
			}
			return "The coding primary retained leadership and answered with the peer finding."
		case "research":
			return `{"result":"The syntax is specified.","findings":["normative section located"],"risks":[],"confidence":0.9}`
		default:
			t.Fatalf("unexpected model %s", name)
			return ""
		}
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, "Fix this syntax handling.", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	events, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKindActor(events, event.PeerTurnCompleted, "research-peer") || !hasKindActor(events, event.FinalCompleted, "deepseek-coder") {
		t.Fatal("consultation did not return to the retaining leader")
	}
	for _, item := range events {
		if item.Kind != event.LeadershipTransferred {
			continue
		}
		data, _ := event.Decode[event.LeadershipTransferredData](item)
		if data.FromAgentID != "" {
			t.Fatalf("consultation unexpectedly moved leadership: %#v", data)
		}
	}
}

func TestRepeatedSpecialistCallsAreFreshAndCallerScoped(t *testing.T) {
	const secret = "ORIGINAL USER CONTEXT MUST NOT LEAK"
	gateway := newScriptedGateway(func(name string, ordinal int, input []*schema.Message) string {
		switch name {
		case "deepseek-code":
			if ordinal == 1 {
				return `{"kind":"coordinate","assessment":"Two independent readings will reduce transcription error.","delegations":[{"key":"read-a","specialist_id":"vision-specialist","objective":"Read the first visible diagnostic from image-1.","context":"Return the exact text only.","attachments":[],"depends_on":[]},{"key":"read-b","specialist_id":"vision-specialist","objective":"Independently read the first visible diagnostic from image-1.","context":"Return the exact text only.","attachments":[],"depends_on":[]}],"peer_turns":[],"cancel":[],"handoff":null}`
			}
			return "Both clean-slate readings agree."
		case "vision":
			joined := allMessageText(input)
			if strings.Contains(joined, secret) {
				t.Fatalf("specialist received implicit user/conversation context: %s", joined)
			}
			if strings.Contains(joined, "clean-slate reading") || strings.Contains(joined, "previous") {
				t.Fatalf("specialist invocation received another invocation's result: %s", joined)
			}
			return fmt.Sprintf(`{"result":"reading-%d","findings":[],"risks":[],"confidence":1}`, ordinal)
		default:
			t.Fatalf("unexpected model %s", name)
			return ""
		}
	})
	ledger, engine, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	run, err := engine.StartAt(ctx, document, secret, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, ctx, ledger, run)
	if gateway.callCount("vision") != 2 {
		t.Fatalf("vision calls = %d, want two isolated invocations", gateway.callCount("vision"))
	}
	events, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range events {
		if item.Kind != event.DelegationCreated {
			continue
		}
		spec, decodeErr := event.Decode[event.DelegationSpec](item)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if spec.CallerID != "deepseek-coder" || spec.SpecialistID != "vision-specialist" {
			t.Fatalf("specialist authority was not caller-scoped: %#v", spec)
		}
	}
}

func TestSpecialistContextScopeCannotRecallAnotherAttempt(t *testing.T) {
	gateway := newScriptedGateway(func(string, int, []*schema.Message) string { return "unused" })
	ledger, _, document, ctx := runtimeHarness(t, gateway, visionCodingProfile(false))
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := ledger.CreateSession(ctx, document, "caller-only task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Append(ctx, session.ID, event.Draft{
		Kind: event.LeadershipTransferred, Actor: "system",
		Data: event.LeadershipTransferredData{ToAgentID: "deepseek-coder", Reason: "primary ingress"},
	}); err != nil {
		t.Fatal(err)
	}
	const delegationID = "018f0000-0000-7000-8000-000000000001"
	if _, err := ledger.Append(ctx, session.ID, event.Draft{
		Kind: event.DelegationCreated, Actor: "deepseek-coder", CorrelationID: delegationID,
		Data: event.DelegationSpec{
			ID: delegationID, Key: "visual-check", CallerID: "deepseek-coder",
			SpecialistID: "vision-specialist", Objective: "Read the explicitly supplied image.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := newSessionRuntime(ledger, nil, session, document, nil)
	owner := "specialist:vision-specialist:" + delegationID + ":2"
	scope, err := runtime.contextScope(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if scope.IncludeAllSessionRecords || len(scope.CorrelationIDs) != 0 {
		t.Fatalf("specialist inherited delegation/session history: %#v", scope)
	}
	if len(scope.Owners) != 1 || scope.Owners[0] != owner {
		t.Fatalf("specialist scope can address another attempt: %#v", scope.Owners)
	}
}

func TestConversationHistoryDoesNotDiscardTurnsBeforeModelBudgeting(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	document, err := profile.FromValue(visionCodingProfile(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	current, err := ledger.CreateSession(ctx, document, "turn 0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const completedTurns = 17
	for turn := 0; turn < completedTurns; turn++ {
		if _, err := ledger.Append(ctx, current.ID, event.Draft{Kind: event.FinalCompleted, Actor: "deepseek-coder", Data: event.FinalCompletedData{Answer: fmt.Sprintf("answer %d", turn)}}); err != nil {
			t.Fatal(err)
		}
		current, err = ledger.CreateContinuation(ctx, current.ID, document, fmt.Sprintf("turn %d", turn+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	runtime := newSessionRuntime(ledger, nil, current, document, nil)
	if err := runtime.loadConversationHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runtime.conversationHistory) != completedTurns {
		t.Fatalf("history turns = %d", len(runtime.conversationHistory))
	}
	for _, turn := range runtime.conversationHistory {
		if turn.Task == "" || turn.Answer == "" || turn.TaskReference == "" || turn.AnswerReference == "" {
			t.Fatalf("turn was eroded before the selected model budget was known: %#v", turn)
		}
	}
}

type scriptedGateway struct {
	mu             sync.Mutex
	counts         map[string]int
	calls          []scriptedCall
	respond        func(string, int, []*schema.Message) string
	respondMessage func(string, int, []*schema.Message) *schema.Message
	models         []string
}

type scriptedCall struct {
	Model    string
	Text     string
	Messages []*schema.Message
}

func newScriptedGateway(respond func(string, int, []*schema.Message) string) *scriptedGateway {
	return &scriptedGateway{
		counts: make(map[string]int), respond: respond,
		models: []string{"deepseek-code", "research", "vision", "architecture"},
	}
}

func (*scriptedGateway) Descriptor() provider.GatewayDescriptor { return testGatewayDescriptor() }

func (g *scriptedGateway) ListModels(context.Context) ([]provider.CatalogModel, error) {
	var result []provider.CatalogModel
	for _, name := range g.models {
		result = append(result, provider.CatalogModel{
			Gateway: "opencode", Route: "test", ID: name,
			Capabilities: provider.Capabilities{ToolCalling: provider.CapabilitySupported},
		})
	}
	return result, nil
}

func (g *scriptedGateway) NewChatModel(_ context.Context, spec profile.Model, _ provider.Mode) (model.BaseChatModel, error) {
	return &scriptedModel{gateway: g, name: spec.Name}, nil
}

func (g *scriptedGateway) invoke(name string, input []*schema.Message) *schema.Message {
	g.mu.Lock()
	g.counts[name]++
	ordinal := g.counts[name]
	g.calls = append(g.calls, scriptedCall{Model: name, Text: allMessageText(input), Messages: cloneModelMessages(input)})
	g.mu.Unlock()
	if g.respondMessage != nil {
		return g.respondMessage(name, ordinal, input)
	}
	return response(g.respond(name, ordinal, input))
}

func (g *scriptedGateway) callCount(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts[name]
}

func (g *scriptedGateway) callsSnapshot() []scriptedCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]scriptedCall(nil), g.calls...)
}

type scriptedModel struct {
	gateway *scriptedGateway
	name    string
}

func (m *scriptedModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scriptedModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.gateway.invoke(m.name, input), nil
}

func (m *scriptedModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.gateway.invoke(m.name, input)}), nil
}

func runtimeHarness(t *testing.T, gateway *scriptedGateway, value profile.Profile) (*store.Store, *Engine, *profile.Document, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	document, err := profile.FromValue(value)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := profile.Validate(document.Profile); profile.HasErrors(diagnostics) {
		t.Fatalf("test Team invalid: %#v", diagnostics)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(gateway); err != nil {
		t.Fatal(err)
	}
	return ledger, NewEngine(ledger, registry), document, ctx
}

func waitRun(t *testing.T, ctx context.Context, ledger *store.Store, run *Run) {
	t.Helper()
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}
}

func visionCodingProfile(sharedSpecialist bool) profile.Profile {
	peerSpecialists := []string(nil)
	if sharedSpecialist {
		peerSpecialists = []string{"vision-specialist"}
	}
	return profile.Profile{
		Schema: profile.CurrentSchema, ID: "vision-assisted-coding", Revision: 1,
		Name: "Vision-assisted coding", Gateway: "opencode",
		Models: map[string]profile.Model{
			"deepseek-model": {Route: "test", Name: "deepseek-code"},
			"research-model": {Route: "test", Name: "research"},
			"vision-model":   {Route: "test", Name: "vision"},
		},
		Primary: profile.AgentAssignment{
			ID: "deepseek-coder", Model: "deepseek-model",
			Definition: "Own implementation end-to-end. This text-only coding model must call the vision specialist when pixels contain required evidence.",
			Peers:      []string{"research-peer"}, Specialists: []string{"vision-specialist"},
		},
		Peers: []profile.AgentAssignment{{
			ID: "research-peer", Model: "research-model",
			Definition:  "Own evidence audits whose deliverable is a sourced conclusion; otherwise contribute as a context-bearing peer.",
			Specialists: peerSpecialists,
		}},
		Specialists: []profile.SpecialistAssignment{{
			ID: "vision-specialist", Model: "vision-model",
			Definition: "Inspect only explicitly attached images and return exact observable visual evidence to the caller.",
		}},
	}
}

func coordinateHandoff(peerID, reason string) string {
	return fmt.Sprintf(`{"kind":"coordinate","assessment":"Transfer ownership.","delegations":[],"peer_turns":[],"cancel":[],"handoff":{"peer_id":%q,"reason":%q}}`, peerID, reason)
}

func exactUserText(messages []*schema.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.User {
			continue
		}
		if strings.HasPrefix(message.Content, "ALT RUNTIME SNAPSHOT ") {
			continue
		}
		if message.Content != "" {
			return message.Content
		}
		var text strings.Builder
		for _, part := range message.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeText {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	return ""
}

func allMessageText(messages []*schema.Message) string {
	var result strings.Builder
	for _, message := range messages {
		if message == nil {
			continue
		}
		result.WriteString(message.Content)
		for _, part := range message.UserInputMultiContent {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func response(content string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant, Content: content,
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20,
		}},
	}
}

func toolCallResponse(name, arguments string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "coordination-call", Type: "function",
			Function: schema.FunctionCall{Name: name, Arguments: arguments},
		}},
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14,
		}},
	}
}

func testGatewayDescriptor() provider.GatewayDescriptor {
	return provider.GatewayDescriptor{
		ID: "opencode", Name: "Purpose-built test gateway",
		CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
		Routes:                []provider.GatewayRoute{{ID: "test", Label: "Test"}}, MultiModelCatalog: true,
	}
}

func hasKindActor(events []event.Event, kind event.Kind, actor string) bool {
	for _, item := range events {
		if item.Kind == kind && item.Actor == actor {
			return true
		}
	}
	return false
}

func logEvents(t *testing.T, ledger *store.Store, sessionID string) {
	t.Helper()
	events, err := ledger.Events(context.Background(), sessionID, 0)
	if err != nil {
		t.Log(err)
		return
	}
	for _, item := range events {
		t.Logf("%03d %-30s %-18s %s", item.Sequence, item.Kind, item.Actor, item.Data)
	}
}

const testProfile = `
schema: 2
id: vision-assisted-coding
revision: 1
name: Vision-assisted coding
gateway: opencode
models:
  deepseek-model: {route: test, name: deepseek-code}
  research-model: {route: test, name: research}
  vision-model: {route: test, name: vision}
primary:
  id: deepseek-coder
  model: deepseek-model
  definition: Own implementation end-to-end; call the vision specialist whenever pixels contain required evidence.
  peers: [research-peer]
  specialists: [vision-specialist]
peers:
  - id: research-peer
    model: research-model
    definition: Own evidence audits whose deliverable is a sourced conclusion; otherwise contribute as a peer.
specialists:
  - id: vision-specialist
    model: vision-model
    definition: Inspect only explicitly attached images and return exact observable visual evidence to the caller.
`
