package thinking_test

import (
	"testing"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
	"altv1/internal/thinking"
)

func TestTeamGraphHasPrimaryIngressPeersAndDirectedSpecialistsWithoutRouter(t *testing.T) {
	p := thinkingProfile()
	projection := thinking.New("conversation", p)
	turn := store.Session{ID: "turn-1", ConversationID: "conversation", Task: "Fix the screenshot error", Status: store.SessionRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := projection.AddTurn(turn); err != nil {
		t.Fatal(err)
	}
	active := projection.Active
	if active.Nodes["router"] != nil {
		t.Fatal("thinking graph still contains a router")
	}
	primary := active.Nodes["member:deepseek-coder"]
	peer := active.Nodes["member:research-peer"]
	specialist := active.Nodes["member:vision-specialist"]
	if primary == nil || peer == nil || specialist == nil {
		t.Fatalf("missing purposeful Team nodes: %#v", active.Nodes)
	}
	if primary.Kind != "agent" || primary.Metadata["primary"] != "true" || peer.Kind != "agent" || specialist.Kind != "specialist" {
		t.Fatalf("incorrect node roles: primary=%#v peer=%#v specialist=%#v", primary, peer, specialist)
	}
	if edge := active.Edges["allowed:user:deepseek-coder"]; edge == nil || edge.From != "user" || edge.To != primary.ID {
		t.Fatalf("user ingress edge = %#v", edge)
	}
	if edge := active.Edges["allowed-peer:deepseek-coder:research-peer"]; edge == nil || edge.Direction != "bidirectional" {
		t.Fatalf("peer edge = %#v", edge)
	}
	if edge := active.Edges["allowed-specialist:deepseek-coder:vision-specialist"]; edge == nil || edge.Direction != "outward" {
		t.Fatalf("specialist permission edge = %#v", edge)
	}
}

func TestLeadershipHandoffAndDirectPeerAnswerAreProjected(t *testing.T) {
	projection := projectionWithTurn(t)
	now := time.Now()
	applyDrafts(t, projection, "turn-1", now, []event.Draft{
		{Kind: event.SessionCreated, Actor: "user", Data: event.SessionCreatedData{Task: "Audit the evidence"}},
		{Kind: event.ProfilePinned, Actor: "system", Data: event.ProfilePinnedData{ProfileID: "vision-coding", Revision: 1}},
		{Kind: event.LeadershipTransferred, Actor: "system", Data: event.LeadershipTransferredData{ToAgentID: "deepseek-coder", Reason: "primary ingress"}},
		{Kind: event.AgentTurnStarted, Actor: "deepseek-coder", Data: event.AgentTurnData{AgentID: "deepseek-coder", Turn: 1}},
		{Kind: event.LeadershipTransferred, Actor: "deepseek-coder", Data: event.LeadershipTransferredData{FromAgentID: "deepseek-coder", ToAgentID: "research-peer", Reason: "evidence is the deliverable"}},
		{Kind: event.AgentTurnStarted, Actor: "research-peer", Data: event.AgentTurnData{AgentID: "research-peer", Turn: 2}},
		{Kind: event.FinalStarted, Actor: "research-peer"},
		{Kind: event.FinalTextDelta, Actor: "research-peer", Data: event.TextDeltaData{Text: "Peer answer"}},
		{Kind: event.FinalCompleted, Actor: "research-peer", Data: event.FinalCompletedData{Answer: "Peer answer"}},
	})
	active := projection.Active
	if active.Status != thinking.Completed {
		t.Fatalf("turn status = %s", active.Status)
	}
	var handoffFound bool
	for _, edge := range active.Edges {
		if edge.Kind == "handoff" && edge.From == "member:deepseek-coder" && edge.To == "member:research-peer" && edge.Metadata["reason"] == "evidence is the deliverable" {
			handoffFound = true
		}
	}
	if !handoffFound {
		t.Fatalf("handoff flow absent: %#v", active.Edges)
	}
	answer := active.Edges["flow:answer"]
	if answer == nil || answer.From != "member:research-peer" || answer.To != "user" {
		t.Fatalf("direct peer answer edge = %#v", answer)
	}
}

func TestSpecialistAndPeerResultsReturnToTheirRecordedCallers(t *testing.T) {
	projection := projectionWithTurn(t)
	now := time.Now()
	applyDrafts(t, projection, "turn-1", now, []event.Draft{
		{Kind: event.SessionCreated, Actor: "user", Data: event.SessionCreatedData{Task: "Fix code from an image"}},
		{Kind: event.ProfilePinned, Actor: "system", Data: event.ProfilePinnedData{ProfileID: "vision-coding", Revision: 1}},
		{Kind: event.LeadershipTransferred, Actor: "system", Data: event.LeadershipTransferredData{ToAgentID: "deepseek-coder", Reason: "primary ingress"}},
		{Kind: event.DelegationCreated, Actor: "deepseek-coder", CorrelationID: "vision-1", Data: event.DelegationSpec{ID: "vision-1", CallerID: "deepseek-coder", SpecialistID: "vision-specialist", Objective: "Read the screenshot"}},
		{Kind: event.DelegationStarted, Actor: "vision-specialist", CorrelationID: "vision-1", Data: event.DelegationStartedData{DelegationID: "vision-1", Attempt: 1}},
		{Kind: event.DelegationCompleted, Actor: "vision-specialist", CorrelationID: "vision-1", Data: event.DelegationCompletedData{DelegationID: "vision-1", Attempt: 1, Result: "undefined symbol"}},
		{Kind: event.PeerTurnCreated, Actor: "deepseek-coder", CorrelationID: "peer-1", Data: event.PeerTurnSpec{ID: "peer-1", CallerID: "deepseek-coder", PeerID: "research-peer", CollaborationID: "spec-check", Objective: "Check the specification", Round: 1}},
		{Kind: event.PeerTurnStarted, Actor: "research-peer", CorrelationID: "peer-1", Data: event.PeerTurnStartedData{PeerTurnID: "peer-1", Attempt: 1}},
		{Kind: event.PeerTurnCompleted, Actor: "research-peer", CorrelationID: "peer-1", Data: event.PeerTurnCompletedData{PeerTurnID: "peer-1", Attempt: 1, Result: "syntax is supported"}},
	})
	var specialistResult, peerResult bool
	for _, edge := range projection.Active.Edges {
		if edge.Kind == "result" && edge.From == "member:vision-specialist" && edge.To == "member:deepseek-coder" {
			specialistResult = true
		}
		if edge.Kind == "peer-result" && edge.From == "member:research-peer" && edge.To == "member:deepseek-coder" {
			peerResult = true
		}
	}
	if !specialistResult || !peerResult {
		t.Fatalf("return flows specialist=%v peer=%v edges=%#v", specialistResult, peerResult, projection.Active.Edges)
	}
}

func TestContextCompactionTargetsAgentAndSpecialistNodes(t *testing.T) {
	projection := projectionWithTurn(t)
	now := time.Now()
	applyDrafts(t, projection, "turn-1", now, []event.Draft{
		{Kind: event.SessionCreated, Actor: "user", Data: event.SessionCreatedData{Task: "Inspect image"}},
		{Kind: event.ProfilePinned, Actor: "system", Data: event.ProfilePinnedData{ProfileID: "vision-coding", Revision: 1}},
		{Kind: event.LeadershipTransferred, Actor: "system", Data: event.LeadershipTransferredData{ToAgentID: "deepseek-coder", Reason: "primary ingress"}},
		{Kind: event.DelegationCreated, Actor: "deepseek-coder", CorrelationID: "vision-1", Data: event.DelegationSpec{ID: "vision-1", CallerID: "deepseek-coder", SpecialistID: "vision-specialist", Objective: "Read image"}},
		{Kind: event.ContextViewCommitted, Actor: "context", Data: event.ContextViewCommittedData{ScopeKind: "agent", ScopeID: "deepseek-coder", Epoch: 2, EstimatedTokens: 400}},
		{Kind: event.ContextViewCommitted, Actor: "context", Data: event.ContextViewCommittedData{ScopeKind: "specialist", ScopeID: "vision-1", Epoch: 1, EstimatedTokens: 80}},
		{Kind: event.ContextAgentCompacted, Actor: "context", CorrelationID: "specialist:vision-specialist:vision-1:1", Data: event.ContextAgentCompactedData{Scope: "specialist:vision-specialist", TranscriptReference: "alt-tool-output://transcript", MessagesBefore: 20, MessagesAfter: 4}},
	})
	primary := projection.Active.Nodes["member:deepseek-coder"]
	specialist := projection.Active.Nodes["member:vision-specialist"]
	if primary.Metadata["context_epoch"] != "2" || specialist.Metadata["context_epoch"] != "1" {
		t.Fatalf("context targets primary=%#v specialist=%#v", primary.Metadata, specialist.Metadata)
	}
	if specialist.Metadata["exact_transcript"] != "alt-tool-output://transcript" {
		t.Fatalf("specialist compaction metadata = %#v", specialist.Metadata)
	}
}

func TestProjectionRejectsSequenceGap(t *testing.T) {
	projection := projectionWithTurn(t)
	item, err := (event.Draft{Kind: event.SessionCreated, Data: event.SessionCreatedData{Task: "x"}}).Materialize("turn-1", 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.Apply(item); err == nil {
		t.Fatal("sequence gap was accepted")
	}
}

func projectionWithTurn(t *testing.T) *thinking.Projection {
	t.Helper()
	now := time.Now()
	projection := thinking.New("conversation", thinkingProfile())
	if err := projection.AddTurn(store.Session{ID: "turn-1", ConversationID: "conversation", Task: "task", Status: store.SessionRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return projection
}

func applyDrafts(t *testing.T, projection *thinking.Projection, sessionID string, at time.Time, drafts []event.Draft) {
	t.Helper()
	for index, draft := range drafts {
		item, err := draft.Materialize(sessionID, int64(index+1), at.Add(time.Duration(index)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if err := projection.Apply(item); err != nil {
			t.Fatal(err)
		}
	}
}

func thinkingProfile() profile.Profile {
	return profile.Profile{
		Schema: profile.CurrentSchema, ID: "vision-coding", Revision: 1, Name: "Vision coding", Gateway: "opencode",
		Models: map[string]profile.Model{
			"deepseek": {Route: "test", Name: "deepseek-code"},
			"research": {Route: "test", Name: "research"},
			"vision":   {Route: "test", Name: "vision"},
		},
		Primary:     profile.AgentAssignment{ID: "deepseek-coder", Model: "deepseek", Definition: "Own code", Peers: []string{"research-peer"}, Specialists: []string{"vision-specialist"}},
		Peers:       []profile.AgentAssignment{{ID: "research-peer", Model: "research", Definition: "Own evidence"}},
		Specialists: []profile.SpecialistAssignment{{ID: "vision-specialist", Model: "vision", Definition: "Read pixels"}},
	}
}
