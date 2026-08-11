package orchestrator

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestStructuredCorrectionMessageNeverEmitsAnEmptyAssistantFrame(t *testing.T) {
	message := structuredCorrectionMessage("")
	if message.Role != schema.Assistant || message.Content == "" {
		t.Fatalf("empty correction message = %#v", message)
	}
	if len(message.ToolCalls) != 0 || message.ReasoningContent != "" {
		t.Fatalf("correction replayed provider-only state: %#v", message)
	}
}

func TestStructuredCorrectionMessageRetainsMalformedTextWithoutToolState(t *testing.T) {
	message := structuredCorrectionMessage("  not-json  ")
	if message.Content != "not-json" {
		t.Fatalf("correction content = %q", message.Content)
	}
	if len(message.ToolCalls) != 0 {
		t.Fatalf("correction retained unmatched tool calls: %#v", message.ToolCalls)
	}
}

func TestMessageTextIncludesStructuredMultiContent(t *testing.T) {
	message := schema.AssistantMessage("", nil)
	message.AssistantGenMultiContent = []schema.MessageOutputPart{{
		Type: schema.ChatMessagePartTypeText,
		Text: `{"lead_id":"engineering"}`,
	}}
	if got := messageText(message); got != `{"lead_id":"engineering"}` {
		t.Fatalf("messageText = %q", got)
	}
}
