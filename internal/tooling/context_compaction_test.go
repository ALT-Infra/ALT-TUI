package tooling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"altv1/internal/provider"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type fixedSummaryModel struct{}

func constrainedTestLimits(tokens int) provider.ModelLimits {
	return provider.ModelLimits{
		ContextWindow: provider.NewTokenLimit(tokens, provider.LimitSourceGatewayCatalog),
	}
}

func constrainedTestBudget(tokens int) *contextBudget {
	return newContextBudget(constrainedTestLimits(tokens))
}

func cacheSafeCompactionBudget(messages []*schema.Message, tools []*schema.ToolInfo) *contextBudget {
	probe := newContextBudget(provider.ModelLimits{})
	estimated := probe.estimateFresh(messages, tools, messages)
	instruction := probe.estimateFresh(
		[]*schema.Message{schema.UserMessage(agentCompactionInstruction)}, nil, messages,
	)
	return constrainedTestBudget(estimated + instruction)
}

func (fixedSummaryModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return fixedSummaryModel{}, nil
}

type capturingSummaryModel struct {
	input []*schema.Message
	tools []*schema.ToolInfo
	calls int
}

func (m *capturingSummaryModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.calls++
	m.input = append([]*schema.Message(nil), input...)
	parsed := model.GetCommonOptions(&model.Options{}, options...)
	m.tools = append([]*schema.ToolInfo(nil), parsed.Tools...)
	return schema.AssistantMessage("cache-aligned summary", nil), nil
}

func TestExactHistoricalToolEvictionRunsBeforeLossyCompaction(t *testing.T) {
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	model := &capturingSummaryModel{}
	handlers, err := runtime.HandlersWithCompaction(context.Background(), "agent:clear-first", model, constrainedTestLimits(60_000))
	if err != nil {
		t.Fatal(err)
	}
	oldResult := strings.Repeat("historical-result\n", 15_000)
	messages := []*schema.Message{
		schema.SystemMessage("stable system"),
		schema.UserMessage("continue the task"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-old", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage(oldResult, "call-old"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-new-1", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage("recent one", "call-new-1"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-new-2", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage("recent two", "call-new-2"),
	}
	state := &adk.ChatModelAgentState{Messages: messages}
	for _, handler := range handlers {
		_, state, err = handler.BeforeModelRewriteState(context.Background(), state, &adk.ModelContext{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if model.calls != 0 {
		t.Fatalf("summarizer calls = %d, want deterministic exact eviction to avoid compaction", model.calls)
	}
	visible := state.Messages[3].Content
	if strings.Contains(visible, "historical-result") || !strings.Contains(visible, toolOutputPathPrefix) {
		t.Fatalf("old result was not replaced by an exact archive reference: %q", visible)
	}
}

func (m *capturingSummaryModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("unused", nil)}), nil
}

func (m *capturingSummaryModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.tools = append([]*schema.ToolInfo(nil), tools...)
	return m, nil
}

func TestCompactionSummaryExtendsExactWarmMessagesAndToolHeader(t *testing.T) {
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	model := &capturingSummaryModel{}
	original := []*schema.Message{schema.SystemMessage("stable system")}
	for index := 0; index < 10; index++ {
		original = append(original, schema.UserMessage(strings.Repeat("stable history ", 2_000)))
	}
	tools := []*schema.ToolInfo{{Name: "zeta", Desc: "z"}, {Name: "alpha", Desc: "a"}}
	middleware, err := runtime.contextCompactionHandler(
		context.Background(), "agent:cache", model, cacheSafeCompactionBudget(original, tools),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := middleware.BeforeModelRewriteState(
		context.Background(), &adk.ChatModelAgentState{Messages: original, ToolInfos: tools}, &adk.ModelContext{},
	); err != nil {
		t.Fatal(err)
	}
	if len(model.input) != len(original)+1 {
		t.Fatalf("summary input messages = %d, want %d", len(model.input), len(original)+1)
	}
	for index := range original {
		if model.input[index] != original[index] {
			t.Fatalf("summary changed warm message %d", index)
		}
	}
	if model.input[len(original)].Role != schema.User || model.input[len(original)].Content != agentCompactionInstruction {
		t.Fatal("compaction instruction was not the sole appended message")
	}
	if len(model.tools) != 2 || model.tools[0].Name != "alpha" || model.tools[1].Name != "zeta" {
		t.Fatalf("summary tool header = %#v", model.tools)
	}
}

func TestSmallMessagesAloneDoNotCausePrematureCompaction(t *testing.T) {
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	model := &capturingSummaryModel{}
	middleware, err := runtime.contextCompactionHandler(context.Background(), "agent:small-messages", model, constrainedTestBudget(60_000))
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("stable system")}
	for index := 0; index < 100; index++ {
		messages = append(messages, schema.UserMessage("short state transition"))
	}
	_, rewritten, err := middleware.BeforeModelRewriteState(
		context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 0 || len(rewritten.Messages) != len(messages) {
		t.Fatalf("small-message compaction changed state: calls=%d messages=%d/%d", model.calls, len(rewritten.Messages), len(messages))
	}
}

func TestDiscoveredCapacityNeverSendsAnOversizedPreventiveSummary(t *testing.T) {
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	const capacity = 8_000
	model := &ceilingSummaryModel{capacity: capacity}
	middleware, err := runtime.contextCompactionHandler(
		context.Background(), "agent:known-capacity", model, constrainedTestBudget(capacity),
	)
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("stable system")}
	for index := 0; index < 12; index++ {
		messages = append(messages, schema.UserMessage(strings.Repeat("older evidence ", 70)))
	}
	messages = append(messages, schema.UserMessage("LATEST EXACT SENTINEL"))
	_, rewritten, err := middleware.BeforeModelRewriteState(
		context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls == 0 || model.oversized != 0 || model.maxInput > capacity {
		t.Fatalf("preventive summary calls=%d oversized=%d max=%d capacity=%d", model.calls, model.oversized, model.maxInput, capacity)
	}
	if !messagesContain(rewritten.Messages, "LATEST EXACT SENTINEL") || len(rewritten.Messages) >= len(messages) {
		t.Fatal("capacity recovery did not retain the recent exact tail while shrinking the working view")
	}
}

func TestToolUnsupportedAgentStillCompactsToTheDiscoveredEnvelope(t *testing.T) {
	var archived []byte
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
		ArchiveToolOutput: func(_ context.Context, _, _ string, content []byte) error {
			archived = append([]byte(nil), content...)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	const capacity = 8_000
	chat := &noToolEnvelopeModel{capacity: capacity}
	handlers, err := runtime.CompactionHandlers(
		context.Background(), "agent:text-only:turn", chat, constrainedTestLimits(capacity),
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name: "text-only-context-scenario", Description: "A model whose catalog rejects tool calling.",
		Instruction: "Answer from the supplied durable state.", Model: chat, Handlers: handlers,
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]*schema.Message, 0, 25)
	for index := 0; index < 24; index++ {
		messages = append(messages, schema.UserMessage(strings.Repeat("older complete runtime evidence ", 90)))
	}
	messages = append(messages, schema.UserMessage("LATEST-TEXT-ONLY-SENTINEL"))
	iterator := agent.Run(context.Background(), &adk.AgentInput{Messages: messages})
	answered := false
	for {
		item, ok := iterator.Next()
		if !ok {
			break
		}
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		if item.Output != nil && item.Output.MessageOutput != nil && item.Output.MessageOutput.Message != nil {
			answered = answered || item.Output.MessageOutput.Message.Content == "text-only answer after bounded recovery"
		}
	}
	if !answered || chat.mainCalls == 0 {
		t.Fatal("the bounded text-only request did not reach the provider and answer")
	}
	if chat.summaryCalls == 0 || chat.oversizedMainCalls != 0 || chat.largestMainInput > capacity {
		t.Fatalf("text-only calls: summaries=%d oversized=%d largest=%d capacity=%d",
			chat.summaryCalls, chat.oversizedMainCalls, chat.largestMainInput, capacity)
	}
	if !bytes.Contains(archived, []byte("older complete runtime evidence")) {
		t.Fatal("text-only compaction failed to archive the exact pre-compaction state")
	}
	if !messagesContain(chat.acceptedMainInput, "LATEST-TEXT-ONLY-SENTINEL") {
		t.Fatal("text-only compaction lost the largest protocol-valid recent exact tail")
	}
}

func TestExactRecallPageFitsTheCurrentModelTrajectoryRatherThanAStaticChunk(t *testing.T) {
	content := strings.Repeat("exact-archive-byte-", 700)
	var requested []int
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		OpenContext: func(_ context.Context, _ string, input ContextOpenInput) (ContextOpenResult, error) {
			requested = append(requested, input.MaxBytes)
			end := len(content)
			if input.MaxBytes > 0 && end-input.ByteOffset > input.MaxBytes {
				end = input.ByteOffset + input.MaxBytes
			}
			return ContextOpenResult{
				Reference: input.Reference, Kind: "tool.completed", Digest: "whole-record-digest",
				ByteCount: len(content), ByteStart: input.ByteOffset, ByteEnd: end,
				HasMore: end < len(content), NextByteOffset: end,
				Encoding: "utf-8", Content: content[input.ByteOffset:end],
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	const owner = "agent:recall-envelope"
	budget := constrainedTestBudget(4_000)
	runtime.setContextBudget(owner, budget)
	plan := budget.plan([]*schema.Message{
		schema.SystemMessage("stable header"),
		schema.UserMessage(strings.Repeat("current working trajectory ", 25)),
	}, nil)
	available := plan.HighWater - plan.Estimated
	if available <= 0 {
		t.Fatalf("test trajectory left no recall envelope: %#v", plan)
	}
	result, err := runtime.openContextForModel(context.Background(), owner, ContextOpenInput{
		Reference: "alt://context/records/018f0000-0000-7000-8000-000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > available || !result.HasMore || result.ByteEnd <= result.ByteStart {
		t.Fatalf("adaptive recall page: wire=%d available=%d range=[%d,%d) more=%t",
			len(wire), available, result.ByteStart, result.ByteEnd, result.HasMore)
	}
	if result.Content != content[:result.ByteEnd] || len(requested) < 2 || requested[0] != available {
		t.Fatalf("recall did not measure its actual serialized envelope: requests=%v result=%#v", requested, result)
	}
}

type noToolEnvelopeModel struct {
	capacity           int
	summaryCalls       int
	mainCalls          int
	oversizedMainCalls int
	largestMainInput   int
	acceptedMainInput  []*schema.Message
}

func (m *noToolEnvelopeModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	size := freshModelInputSize(input, options...)
	isSummary := len(input) > 0 && input[len(input)-1] != nil && input[len(input)-1].Content == agentCompactionInstruction
	if isSummary {
		m.summaryCalls++
		if size > m.capacity {
			return nil, &provider.Failure{
				Kind: provider.FailureContextOverflow, ContextLimit: m.capacity,
				Err: errors.New("summary exceeded authenticated text-only model capacity"),
			}
		}
		return schema.AssistantMessage("The older state is exact and addressable; preserve the latest instruction.", nil), nil
	}
	m.mainCalls++
	m.largestMainInput = max(m.largestMainInput, size)
	if size > m.capacity {
		m.oversizedMainCalls++
		return nil, &provider.Failure{
			Kind: provider.FailureContextOverflow, ContextLimit: m.capacity,
			Err: errors.New("main call exceeded authenticated text-only model capacity"),
		}
	}
	m.acceptedMainInput = append([]*schema.Message(nil), input...)
	return schema.AssistantMessage("text-only answer after bounded recovery", nil), nil
}

func (m *noToolEnvelopeModel) Stream(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mainCalls++
	size := freshModelInputSize(input, options...)
	m.largestMainInput = max(m.largestMainInput, size)
	if size > m.capacity {
		m.oversizedMainCalls++
		return nil, &provider.Failure{
			Kind: provider.FailureContextOverflow, ContextLimit: m.capacity,
			Err: errors.New("main call exceeded authenticated text-only model capacity"),
		}
	}
	m.acceptedMainInput = append([]*schema.Message(nil), input...)
	return schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("text-only answer after bounded recovery", nil),
	}), nil
}

func freshModelInputSize(input []*schema.Message, options ...model.Option) int {
	parsed := model.GetCommonOptions(&model.Options{}, options...)
	return newContextBudget(provider.ModelLimits{}).estimateFresh(input, parsed.Tools, input)
}

type ceilingSummaryModel struct {
	capacity  int
	calls     int
	oversized int
	maxInput  int
}

func (m *ceilingSummaryModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.calls++
	parsed := model.GetCommonOptions(&model.Options{}, options...)
	probe := newContextBudget(provider.ModelLimits{})
	size := probe.estimateFresh(input, parsed.Tools, input)
	m.maxInput = max(m.maxInput, size)
	if size > m.capacity {
		m.oversized++
		return nil, &provider.Failure{
			Kind: provider.FailureContextOverflow, ContextLimit: m.capacity,
			Err: errors.New("preventive summary crossed discovered capacity"),
		}
	}
	return schema.AssistantMessage("Bounded continuation brief.", nil), nil
}

func (m *ceilingSummaryModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("unused", nil)}), nil
}

func (fixedSummaryModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("The active objective and unresolved work remain pinned.", nil), nil
}

func (fixedSummaryModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("unused", nil)}), nil
}

func TestAgentCompactionArchivesExactPermittedTranscriptBeforeReplacement(t *testing.T) {
	var indexedReference string
	var indexedContent []byte
	var compactedReference string
	var beforeCount, afterCount int
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
		ArchiveToolOutput: func(_ context.Context, owner, reference string, content []byte) error {
			if owner != "peer:research:collaboration-7" {
				t.Fatalf("archive owner = %q", owner)
			}
			indexedReference, indexedContent = reference, append([]byte(nil), content...)
			return nil
		},
		RecordAgentCompaction: func(_ context.Context, record AgentCompactionRecord) error {
			if record.Scope != "peer:research:collaboration-7" || record.Trigger != "working_budget" || record.PromptCapacity <= 0 {
				t.Fatalf("compaction derivation = %#v", record)
			}
			compactedReference, beforeCount, afterCount = record.TranscriptReference, record.MessagesBefore, record.MessagesAfter
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	messages := []*schema.Message{schema.SystemMessage("stable system")}
	for index := 0; index < 10; index++ {
		messages = append(messages, schema.UserMessage(strings.Repeat("observable-needle ", 1_500)))
	}
	messages = append(messages, &schema.Message{
		Role: schema.Assistant, Content: "observable answer", ReasoningContent: "private-reasoning-must-not-persist",
	})
	messages = append(messages, schema.UserMessage(strings.Repeat("latest growth trajectory ", 1_500)))
	middleware, err := runtime.contextCompactionHandler(
		context.Background(), "peer:research:collaboration-7", fixedSummaryModel{}, cacheSafeCompactionBudget(messages, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, rewritten, err := middleware.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	if indexedReference == "" || indexedReference != compactedReference || beforeCount != len(messages) || afterCount != len(rewritten.Messages) {
		t.Fatalf("compaction lifecycle = ref %q/%q counts %d/%d, want %d/%d", indexedReference, compactedReference, beforeCount, afterCount, len(messages), len(rewritten.Messages))
	}
	if !bytes.Contains(indexedContent, []byte("observable-needle")) {
		t.Fatal("exact permitted transcript content was not archived")
	}
	if bytes.Contains(indexedContent, []byte("private-reasoning-must-not-persist")) {
		t.Fatal("provider reasoning crossed the disabled persistence policy")
	}
	if len(rewritten.Messages) >= len(messages) || !messagesContain(rewritten.Messages, indexedReference) {
		t.Fatal("bounded replacement omitted its exact transcript reference")
	}
}

func TestRepeatedAgentCompactionFormsAddressableChainInsteadOfSummaryErosion(t *testing.T) {
	archives := make(map[string][]byte)
	var references []string
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
		ArchiveToolOutput: func(_ context.Context, _ string, reference string, content []byte) error {
			archives[reference] = append([]byte(nil), content...)
			return nil
		},
		RecordAgentCompaction: func(_ context.Context, record AgentCompactionRecord) error {
			references = append(references, record.TranscriptReference)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	middleware, err := runtime.contextCompactionHandler(context.Background(), "agent:systems:4", fixedSummaryModel{}, constrainedTestBudget(60_000))
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("stable")}
	for index := 0; index < 10; index++ {
		messages = append(messages, schema.UserMessage(strings.Repeat("first-era-evidence ", 1_500)))
	}
	_, first, err := middleware.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	secondInput := append([]*schema.Message(nil), first.Messages...)
	for index := 0; index < 10; index++ {
		secondInput = append(secondInput, schema.UserMessage(strings.Repeat("second-era-evidence ", 1_500)))
	}
	_, second, err := middleware.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{Messages: secondInput}, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 || references[0] == references[1] {
		t.Fatalf("repeated compaction references = %#v", references)
	}
	if !bytes.Contains(archives[references[0]], []byte("first-era-evidence")) ||
		!bytes.Contains(archives[references[1]], []byte(references[0])) ||
		!bytes.Contains(archives[references[1]], []byte("second-era-evidence")) {
		t.Fatal("repeated compaction did not preserve an exact addressable lineage")
	}
	if len(second.Messages) >= len(secondInput) || !messagesContain(second.Messages, references[1]) {
		t.Fatal("second bounded view did not replace growth with the newest exact address")
	}
}

func messagesContain(messages []*schema.Message, expected string) bool {
	for _, message := range messages {
		if message != nil && strings.Contains(message.Content, expected) {
			return true
		}
	}
	return false
}

func TestAgentCompactionArchivesAttachmentAddressWithoutDuplicatingBinary(t *testing.T) {
	var archived []byte
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
		ArchiveToolOutput: func(_ context.Context, _, _ string, content []byte) error {
			archived = append([]byte(nil), content...)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	encoded := strings.Repeat("sensitive-image-base64", 100)
	messages := []*schema.Message{{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: "path /durable/image.png"},
		{Type: schema.ChatMessagePartTypeImageURL, Extra: map[string]any{
			"alt_artifact_reference": "artifact:immutable",
		}, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
			Base64Data: &encoded, MIMEType: "image/png",
		}}},
	}}}
	if _, err := runtime.archiveAgentTranscript(context.Background(), "agent:test", messages); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archived, []byte("sensitive-image-base64")) {
		t.Fatal("compaction archive duplicated immutable attachment bytes")
	}
	if !bytes.Contains(archived, []byte("artifact:immutable")) || !bytes.Contains(archived, []byte("/durable/image.png")) {
		t.Fatal("compaction archive lost the exact attachment address")
	}
}

func TestOverflowRecoveryContinuesTheEinoToolLoopWithoutReplayingWork(t *testing.T) {
	var archivedReferences []string
	var archivedContents [][]byte
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ContextArchiveDirectory: t.TempDir(),
		ArchiveToolOutput: func(_ context.Context, _ string, reference string, content []byte) error {
			archivedReferences = append(archivedReferences, reference)
			archivedContents = append(archivedContents, append([]byte(nil), content...))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	scenario := &overflowToolLoopModel{}
	compactor, err := runtime.contextCompactionHandler(
		context.Background(), "peer:implementation:overflow-scenario", scenario, newContextBudget(provider.ModelLimits{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	var toolCalls atomic.Int32
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name: "overflow-scenario", Description: "exercise overflow recovery across a real Eino tool loop",
		Instruction: "Preserve exact recent evidence and use the verification tool once.",
		Model:       scenario, Handlers: []adk.ChatModelAgentMiddleware{compactor},
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{&countingVerificationTool{calls: &toolCalls}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	messages := make([]*schema.Message, 0, 9)
	for index := 0; index < 7; index++ {
		messages = append(messages, schema.UserMessage(strings.Repeat("older exact evidence ", 180)))
	}
	messages = append(messages, schema.UserMessage("RECENT-SENTINEL: keep this exact instruction verbatim"))
	iterator := agent.Run(context.Background(), &adk.AgentInput{Messages: messages})
	var final string
	for {
		item, ok := iterator.Next()
		if !ok {
			break
		}
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		if item.Output != nil && item.Output.MessageOutput != nil && item.Output.MessageOutput.Message != nil {
			message := item.Output.MessageOutput.Message
			if message.Role == schema.Assistant && len(message.ToolCalls) == 0 {
				final = message.Content
			}
		}
	}

	if scenario.overflowCount() != 1 {
		t.Fatalf("provider overflows = %d, want one learned rejection", scenario.overflowCount())
	}
	if scenario.summaryCount() < 2 {
		t.Fatalf("summary attempts = %d, provider-directed retry did not reach success", scenario.summaryCount())
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("verification tool calls = %d, work was skipped or replayed", toolCalls.Load())
	}
	if final != "verified after recovery" {
		t.Fatalf("final answer = %q", final)
	}
	if len(archivedReferences) == 0 || !bytes.Contains(archivedContents[0], []byte("older exact evidence")) {
		t.Fatal("overflow recovery did not preserve the exact rejected transcript")
	}
	accepted := scenario.firstAcceptedMainInput()
	acceptedReference := false
	for _, reference := range archivedReferences {
		acceptedReference = acceptedReference || messagesContain(accepted, reference)
	}
	if !acceptedReference || !messagesContain(accepted, "RECENT-SENTINEL: keep this exact instruction verbatim") {
		t.Fatalf("recovered agent input lost its exact archive address or verbatim recent tail: ref=%t tail=%t messages=%s",
			acceptedReference,
			messagesContain(accepted, "RECENT-SENTINEL: keep this exact instruction verbatim"),
			allMessageContents(accepted),
		)
	}
}

func allMessageContents(messages []*schema.Message) string {
	var parts []string
	for _, message := range messages {
		if message != nil {
			parts = append(parts, string(message.Role)+":"+message.Content)
		}
	}
	return strings.Join(parts, " | ")
}

type overflowToolLoopModel struct {
	mu             sync.Mutex
	mainCalls      int
	summaryCalls   int
	overflows      int
	acceptedInputs [][]*schema.Message
}

func (m *overflowToolLoopModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *overflowToolLoopModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.respond(input)
}

func (m *overflowToolLoopModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.respond(input)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func (m *overflowToolLoopModel) respond(input []*schema.Message) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(input) > 0 && input[len(input)-1] != nil && input[len(input)-1].Content == agentCompactionInstruction {
		m.summaryCalls++
		if m.summaryCalls == 1 {
			return nil, &provider.Failure{
				Kind: provider.FailureRateLimited, RetryAfter: time.Nanosecond,
				Err: errors.New("summary capacity temporarily unavailable"),
			}
		}
		return schema.AssistantMessage("Older evidence was archived; preserve the exact recent instruction and verify once.", nil), nil
	}

	m.mainCalls++
	if m.mainCalls == 1 {
		m.overflows++
		return nil, &provider.Failure{
			Kind: provider.FailureContextOverflow, ContextLimit: 8_000,
			Err: errors.New("maximum context length is 8000 tokens"),
		}
	}
	copyInput := make([]*schema.Message, len(input))
	copy(copyInput, input)
	m.acceptedInputs = append(m.acceptedInputs, copyInput)
	for _, message := range input {
		if message != nil && message.Role == schema.Tool && message.ToolCallID == "verify-after-overflow" {
			return schema.AssistantMessage("verified after recovery", nil), nil
		}
	}
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: "verify-after-overflow", Type: "function",
		Function: schema.FunctionCall{Name: "verify_recovery", Arguments: `{}`},
	}}), nil
}

func (m *overflowToolLoopModel) overflowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.overflows
}

func (m *overflowToolLoopModel) summaryCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.summaryCalls
}

func (m *overflowToolLoopModel) firstAcceptedMainInput() []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.acceptedInputs) == 0 {
		return nil
	}
	return append([]*schema.Message(nil), m.acceptedInputs[0]...)
}

type countingVerificationTool struct {
	calls *atomic.Int32
}

func (t *countingVerificationTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "verify_recovery", Desc: "Records one post-recovery verification."}, nil
}

func (t *countingVerificationTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	t.calls.Add(1)
	return `{"verified":true}`, nil
}

var _ tool.InvokableTool = (*countingVerificationTool)(nil)
