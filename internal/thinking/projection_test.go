package thinking_test

import (
	"strings"
	"testing"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
	"altv1/internal/thinking"
)

func TestProjectionShowsParallelWorkAndIndependentReturn(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "parallel"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "research-a", MemberID: "research", Objective: "find evidence",
		}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "verify-b", MemberID: "verification", Objective: "independent check",
		}},
		event.Draft{Kind: event.DelegationStarted, Data: event.DelegationStartedData{
			DelegationID: "research-a", Attempt: 1,
		}},
		event.Draft{Kind: event.DelegationStarted, Data: event.DelegationStartedData{
			DelegationID: "verify-b", Attempt: 1,
		}},
	)

	research := projection.Active.Edges["flow:delegation:engineering:research"]
	verification := projection.Active.Edges["flow:delegation:engineering:verification"]
	if research == nil || research.Active != 1 || research.Direction != "outward" {
		t.Fatalf("research flow = %#v", research)
	}
	if verification == nil || verification.Active != 1 || verification.Direction != "outward" {
		t.Fatalf("verification flow = %#v", verification)
	}

	apply(t, projection, event.Draft{
		Kind: event.DelegationCompleted,
		Data: event.DelegationCompletedData{
			DelegationID: "research-a", Attempt: 1, Result: "evidence",
		},
	})
	if research.Active != 0 {
		t.Fatalf("completed research active count = %d, want 0", research.Active)
	}
	if verification.Active != 1 {
		t.Fatalf("independent verification was incorrectly closed: %#v", verification)
	}
	returned := projection.Active.Edges["flow:result:research:engineering"]
	if returned == nil || returned.Direction != "inward" || returned.Status != thinking.Completed {
		t.Fatalf("return flow = %#v", returned)
	}
}

func TestDependencyRemainsQueuedUntilARecordedStart(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "sequential"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "first", MemberID: "research", Objective: "first",
		}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "second", MemberID: "verification", Objective: "second",
			DependsOn: []string{"first"},
		}},
		event.Draft{Kind: event.DelegationStarted, Data: event.DelegationStartedData{
			DelegationID: "first", Attempt: 1,
		}},
	)
	second := projection.Active.Edges["flow:delegation:engineering:verification"]
	if second == nil || second.Status != thinking.Queued || second.Active != 0 {
		t.Fatalf("dependent work was displayed as released without a start event: %#v", second)
	}
}

func TestASecondTurnAdvancesOneSessionProjection(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "first"}},
		event.Draft{Kind: event.SessionFailed, Data: event.FailureData{Error: "failed"}},
	)
	firstID := projection.ActiveTurnID
	second := store.Session{
		ID: "turn-2", ConversationID: projection.SessionID, Task: "second",
		Status: store.SessionRunning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := projection.AddTurn(second); err != nil {
		t.Fatal(err)
	}
	item, err := (event.Draft{
		Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "second"},
	}).Materialize(second.ID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.Apply(item); err != nil {
		t.Fatal(err)
	}
	if projection.SessionID != "conversation" {
		t.Fatalf("session identity changed to %q", projection.SessionID)
	}
	if projection.ActiveTurnID != second.ID || len(projection.Turns) != 2 {
		t.Fatalf("second turn did not advance the session projection: %#v", projection)
	}
	if projection.Turns[0].ID != firstID || projection.Turns[0].Status != thinking.Failed {
		t.Fatalf("first turn history was not retained: %#v", projection.Turns[0])
	}
}

func TestProjectionRejectsAnUnexplainedSequenceGap(t *testing.T) {
	projection := testProjection(t)
	item, err := (event.Draft{Kind: event.RouterStarted}).Materialize("turn-1", 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.Apply(item); err == nil {
		t.Fatal("sequence 2 was accepted before sequence 1")
	}
	if projection.Active.Sequence != 0 {
		t.Fatalf("projection advanced across a gap to %d", projection.Active.Sequence)
	}
}

func TestTerminalFailureClosesEveryOpenActivity(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "fail"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "research-a", MemberID: "research", Objective: "open work",
		}},
		event.Draft{Kind: event.DelegationStarted, Data: event.DelegationStartedData{
			DelegationID: "research-a", Attempt: 1,
		}},
		event.Draft{Kind: event.FinalStarted},
		event.Draft{Kind: event.SessionFailed, Data: event.FailureData{Error: "endpoint stopped"}},
	)
	for _, edge := range projection.Active.Edges {
		if edge.Active != 0 {
			t.Fatalf("terminal turn retains active edge %#v", edge)
		}
	}
	for _, node := range projection.Active.Nodes {
		if node.Status == thinking.Running || node.Status == thinking.Queued {
			t.Fatalf("terminal turn retains open node %#v", node)
		}
	}
	if projection.Active.Status != thinking.Failed {
		t.Fatalf("turn status = %q", projection.Active.Status)
	}
}

func TestProjectionRetainsCompleteMetadataForGeometryAwarePresentation(t *testing.T) {
	projection := testProjection(t)
	longArguments := strings.Repeat("complete-argument-", 4096)
	longResult := strings.Repeat("complete-result-", 4096)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "inspect"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.ToolCalled, Actor: "engineering", Data: event.ToolCallData{
			ToolCallID: "tool-long", Tool: "inspect", Arguments: longArguments,
		}},
		event.Draft{Kind: event.ToolCompleted, Actor: "engineering", Data: event.ToolCompletedData{
			ToolCallID: "tool-long", Tool: "inspect", Result: longResult,
		}},
	)
	node := projection.Active.Nodes["tool:tool-long"]
	if node == nil {
		t.Fatal("tool node missing")
	}
	if got := node.Metadata["arguments"]; got != longArguments {
		t.Fatalf("metadata was changed or truncated: got %d bytes, want %d", len(got), len(longArguments))
	}
	if got := node.Metadata["result"]; got != longResult {
		t.Fatalf("tool result was changed or truncated: got %d bytes, want %d", len(got), len(longResult))
	}
}

func TestProjectionDistinguishesToolDiscoveryAndResearchProvider(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "research"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.ToolCalled, Actor: "engineering", Data: event.ToolCallData{
			ToolCallID: "discover", Tool: "tool_search", Arguments: `{"query":"web evidence"}`,
		}},
		event.Draft{Kind: event.ToolCompleted, Actor: "engineering", Data: event.ToolCompletedData{
			ToolCallID: "discover", Tool: "tool_search", Result: `{"tools":["web_search"]}`,
		}},
		event.Draft{Kind: event.ToolCalled, Actor: "engineering", Data: event.ToolCallData{
			ToolCallID: "search", Tool: "web_search", Provider: "linkup", Arguments: `{"query":"primary evidence"}`,
		}},
		event.Draft{Kind: event.ToolCompleted, Actor: "engineering", Data: event.ToolCompletedData{
			ToolCallID: "search", Tool: "web_search", Result: `{"provider":"linkup","mode":"search"}`,
		}},
	)
	discovery := projection.Active.Nodes["tool:discover"]
	if discovery == nil || discovery.Kind != "tool-discovery" {
		t.Fatalf("discovery node = %#v", discovery)
	}
	if edge := projection.Active.Edges["flow:tool:discover"]; edge == nil || edge.Kind != "tool-discovery" {
		t.Fatalf("discovery edge = %#v", edge)
	}
	search := projection.Active.Nodes["tool:search"]
	if search == nil || search.Kind != "tool" || search.Metadata["provider"] != "Linkup" {
		t.Fatalf("research node = %#v", search)
	}
}

func TestProjectionAttachesContextLifecycleToItsActualScope(t *testing.T) {
	projection := testProjection(t)
	apply(t, projection,
		event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "context"}},
		event.Draft{Kind: event.RouterStarted},
		event.Draft{Kind: event.LeadSelected, Data: event.LeadSelectedData{LeadID: "engineering"}},
		event.Draft{Kind: event.DelegationCreated, Data: event.DelegationSpec{
			ID: "research-call", MemberID: "research", Objective: "inspect evidence",
		}},
		event.Draft{Kind: event.ContextViewCommitted, Data: event.ContextViewCommittedData{
			ScopeKind: "specialist", ScopeID: "research-call", Epoch: 4,
			EstimatedTokens: 8192, ViewDigest: "specialist-view", Compacted: true,
		}},
		event.Draft{Kind: event.ContextAgentCompacted, CorrelationID: "research-call", Data: event.ContextAgentCompactedData{
			Scope: "member:research", TranscriptReference: "alt-tool-output://research-transcript",
			MessagesBefore: 84, MessagesAfter: 6,
		}},
		event.Draft{Kind: event.PeerTurnCreated, Data: event.PeerTurnSpec{
			ID: "peer-turn-1", CollaborationID: "collaboration-1", PeerID: "verification",
			Objective: "challenge the finding", Round: 1,
		}},
		event.Draft{Kind: event.ContextViewCommitted, Data: event.ContextViewCommittedData{
			ScopeKind: "peer", ScopeID: "collaboration-1", Epoch: 2,
			EstimatedTokens: 4096, ViewDigest: "peer-view",
		}},
		event.Draft{Kind: event.ContextAgentCompacted, CorrelationID: "peer-turn-1", Data: event.ContextAgentCompactedData{
			Scope: "peer:verification", TranscriptReference: "alt-tool-output://peer-transcript",
			MessagesBefore: 82, MessagesAfter: 5,
		}},
	)

	research := projection.Active.Nodes["member:research"]
	if research == nil || research.Metadata["context_epoch"] != "4" ||
		research.Metadata["projection_compactions"] != "1" ||
		research.Metadata["exact_transcript"] != "alt-tool-output://research-transcript" ||
		research.Metadata["messages_before_compaction"] != "84" {
		t.Fatalf("specialist context metadata = %#v", research)
	}
	verification := projection.Active.Nodes["member:verification"]
	if verification == nil || verification.Metadata["context_epoch"] != "2" ||
		verification.Metadata["working_view_digest"] != "peer-view" ||
		verification.Metadata["exact_transcript"] != "alt-tool-output://peer-transcript" ||
		verification.Metadata["messages_after_compaction"] != "5" {
		t.Fatalf("peer context metadata = %#v", verification)
	}
	if lead := projection.Active.Nodes["member:engineering"]; lead.Metadata["exact_transcript"] != "" {
		t.Fatalf("specialist or peer compaction was attached to Lead: %#v", lead.Metadata)
	}
}

func testProjection(t *testing.T) *thinking.Projection {
	t.Helper()
	value := profile.Profile{
		Leads: []profile.LeadAssignment{{
			ID: "engineering", Calls: []string{"research", "verification"},
		}},
		Members: []profile.MemberAssignment{
			{ID: "research"},
			{ID: "verification"},
		},
	}
	projection := thinking.New("conversation", value)
	record := store.Session{
		ID: "turn-1", ConversationID: "conversation", Task: "task",
		Status: store.SessionRunning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := projection.AddTurn(record); err != nil {
		t.Fatal(err)
	}
	return projection
}

func apply(t *testing.T, projection *thinking.Projection, drafts ...event.Draft) {
	t.Helper()
	for _, draft := range drafts {
		item, err := draft.Materialize(
			projection.ActiveTurnID,
			projection.Active.Sequence+1,
			time.Now(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := projection.Apply(item); err != nil {
			t.Fatal(err)
		}
	}
}
