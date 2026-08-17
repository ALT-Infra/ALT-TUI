package orchestrator

import (
	"sort"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

func TestModelSurfaceAppendsSnapshotsAndNewUserTurnsWithoutRewritingPrefix(t *testing.T) {
	const system = "stable role"
	surface := &modelSurface{
		Version: modelSurfaceVersion, ConversationID: "conversation", AgentID: "coder",
	}
	first, firstSnapshot, err := prepareModelSurfaceMessages(
		surface, system, "turn-1", schema.UserMessage("FIRST EXACT REQUEST"), `{"work":1}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Content != "FIRST EXACT REQUEST" || first[1].Content == `{"work":1}` {
		t.Fatalf("first request tail = %#v", first)
	}
	surface.Messages = append([]*schema.Message{schema.SystemMessage(system)}, first...)
	surface.Messages = append(surface.Messages, schema.AssistantMessage("tool requested", nil), schema.ToolMessage("evidence", "call-1", schema.WithToolName("read")))
	surface.LastSessionID = "turn-1"
	surface.LastSnapshotDigest = firstSnapshot
	previous := cloneModelMessages(surface.Messages)

	second, secondSnapshot, err := prepareModelSurfaceMessages(
		surface, system, "turn-1", schema.UserMessage("FIRST EXACT REQUEST"), `{"work":2}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := append([]*schema.Message{schema.SystemMessage(system)}, second...)
	if !modelMessagesHavePrefix(secondRequest, previous) {
		t.Fatal("same-turn runtime update rewrote the previous model-visible prefix")
	}
	if secondSnapshot == firstSnapshot || second[len(second)-1].Role != schema.User {
		t.Fatal("changed durable state was not appended as a new user-role snapshot")
	}

	surface.Messages = append(cloneModelMessages(secondRequest), schema.AssistantMessage("first answer", nil))
	surface.LastSnapshotDigest = secondSnapshot
	previous = cloneModelMessages(surface.Messages)
	third, _, err := prepareModelSurfaceMessages(
		surface, system, "turn-2", schema.UserMessage("SECOND EXACT REQUEST"), `{"work":3}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	thirdRequest := append([]*schema.Message{schema.SystemMessage(system)}, third...)
	if !modelMessagesHavePrefix(thirdRequest, previous) {
		t.Fatal("new user turn rewrote the context-bearing agent's previous prefix")
	}
	if third[len(previous)-1].Content != "SECOND EXACT REQUEST" {
		t.Fatalf("new exact user message was not appended word-for-word: %#v", third[len(previous)-1])
	}
}

func TestCanonicalToolOrderingProducesOneHeaderDigest(t *testing.T) {
	messages := []*schema.Message{schema.SystemMessage("stable")}
	a := &schema.ToolInfo{Name: "alpha", Desc: "a"}
	z := &schema.ToolInfo{Name: "zeta", Desc: "z"}
	left := digestModelHeader("model", messages, []*schema.ToolInfo{z, a}, nil)
	right := digestModelHeader("model", messages, []*schema.ToolInfo{a, z}, nil)
	if left != right {
		t.Fatalf("tool ordering changed header digest: %s != %s", left, right)
	}
}

func TestHeaderDigestMatchesEinoRequiredArrayCanonicalization(t *testing.T) {
	parameters := &jsonschema.Schema{Type: string(schema.Object), Required: []string{"zeta", "alpha"}}
	tool := &schema.ToolInfo{Name: "example", ParamsOneOf: schema.NewParamsOneOfByJSONSchema(parameters)}
	messages := []*schema.Message{schema.SystemMessage("stable")}
	before := digestModelHeader("model", messages, []*schema.ToolInfo{tool}, nil)
	// Eino's OpenAI adapter performs this mutation immediately before it
	// serializes the same tool schema onto the wire.
	sort.Strings(parameters.Required)
	after := digestModelHeader("model", messages, []*schema.ToolInfo{tool}, nil)
	if before != after {
		t.Fatalf("wire-equivalent required ordering changed cache header digest: %s != %s", before, after)
	}
}

func TestModelSurfaceCommitDigestSkipsOnlyIdenticalState(t *testing.T) {
	surface := &modelSurface{
		Version: modelSurfaceVersion, ConversationID: "conversation", AgentID: "coder",
		Epoch: 1, Messages: []*schema.Message{schema.SystemMessage("stable"), schema.UserMessage("task")},
	}
	first, err := modelSurfaceCommitDigest(surface)
	if err != nil {
		t.Fatal(err)
	}
	surface.CommitDigest = first
	second, err := modelSurfaceCommitDigest(surface)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("commit digest depended on its own persisted digest")
	}
	surface.Messages = append(surface.Messages, schema.AssistantMessage("answer", nil))
	third, err := modelSurfaceCommitDigest(surface)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("changed model-visible history retained the old commit digest")
	}
}
