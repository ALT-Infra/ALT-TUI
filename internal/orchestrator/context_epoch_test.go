package orchestrator

import (
	"context"
	"testing"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
	"altv1/internal/tooling"

	"github.com/cloudwego/eino/schema"
)

func TestWorkingViewCompactionDetectionUsesJSONSemantics(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   bool
	}{
		{name: "compacted result", prompt: `{"delegations":[{"compacted":true}]}`, want: true},
		{name: "archived evidence", prompt: `{"archived_evidence":{"omitted_entries":1}}`, want: true},
		{name: "nested archive", prompt: `{"team_evidence":{"archived":{"omitted_entries":2}}}`, want: true},
		{name: "archived rounds", prompt: `{"archived_rounds":3}`, want: true},
		{name: "empty markers", prompt: `{"compacted":false,"archived_evidence":null,"archived":null,"archived_rounds":0}`},
		{name: "marker text", prompt: `{"result":"the word compacted is not state"}`},
		{name: "invalid", prompt: `not json`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workingViewWasCompacted(test.prompt); got != test.want {
				t.Fatalf("workingViewWasCompacted() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCompactionLifecycleAccountsForItsInternalModelCall(t *testing.T) {
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
	session, err := ledger.CreateSession(ctx, document, "exercise compaction accounting", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := newSessionRuntime(ledger, nil, session, document, nil)
	summary := schema.AssistantMessage("continuation brief", nil)
	summary.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens: 1_200, CompletionTokens: 80, TotalTokens: 1_280,
	}}
	if err := runtime.recordAgentCompaction(ctx, tooling.AgentCompactionRecord{
		Scope: "peer:research-peer:consultation:1", Trigger: "provider_overflow",
		TranscriptReference: "alt-tool-output://exact", MessagesBefore: 20, MessagesAfter: 5,
		EstimatedTokens: 4_100, PromptCapacity: 4_000, HighWater: 4_000, Summary: summary,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := ledger.Events(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var compacted event.ContextAgentCompactedData
	var usage event.ModelUsageData
	for _, item := range items {
		switch item.Kind {
		case event.ContextAgentCompacted:
			compacted, err = event.Decode[event.ContextAgentCompactedData](item)
		case event.ModelUsage:
			usage, err = event.Decode[event.ModelUsageData](item)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if compacted.Trigger != "provider_overflow" || compacted.EstimatedTokens != 4_100 || compacted.PromptCapacity != 4_000 {
		t.Fatalf("compaction derivation was not persisted: %#v", compacted)
	}
	if usage.Model != "research-model:research" || usage.Purpose != "context-compaction:peer:research-peer:consultation:1" || usage.TotalTokens != 1_280 {
		t.Fatalf("internal compaction usage remained hidden: %#v", usage)
	}
}
