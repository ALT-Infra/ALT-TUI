package tooling

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fixedSummaryModel struct{}

func (fixedSummaryModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return fixedSummaryModel{}, nil
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
		RecordAgentCompaction: func(_ context.Context, owner, reference string, before, after int) error {
			compactedReference, beforeCount, afterCount = reference, before, after
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	middleware, err := runtime.contextCompactionHandler(context.Background(), "peer:research:collaboration-7", fixedSummaryModel{})
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("stable system")}
	for index := 0; index < 81; index++ {
		messages = append(messages, schema.UserMessage("observable-needle"))
	}
	messages = append(messages, &schema.Message{
		Role: schema.Assistant, Content: "observable answer", ReasoningContent: "private-reasoning-must-not-persist",
	})
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
	if len(rewritten.Messages) >= len(messages) || !strings.Contains(rewritten.Messages[len(rewritten.Messages)-1].Content, indexedReference) {
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
		RecordAgentCompaction: func(_ context.Context, _ string, reference string, _, _ int) error {
			references = append(references, reference)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	middleware, err := runtime.contextCompactionHandler(context.Background(), "lead:systems:4", fixedSummaryModel{})
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("stable")}
	for index := 0; index < 81; index++ {
		messages = append(messages, schema.UserMessage("first-era-evidence"))
	}
	_, first, err := middleware.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	secondInput := append([]*schema.Message(nil), first.Messages...)
	for index := 0; index < 81; index++ {
		secondInput = append(secondInput, schema.UserMessage("second-era-evidence"))
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
	if len(second.Messages) >= len(secondInput) || !strings.Contains(second.Messages[len(second.Messages)-1].Content, references[1]) {
		t.Fatal("second bounded view did not replace growth with the newest exact address")
	}
}
