package orchestrator

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/profile"
)

func TestProjectionPreservesCompleteEvidenceUntilTheModelBudgetLayer(t *testing.T) {
	state := &Projection{SessionID: "long-running-turn", Delegations: map[string]*Delegation{}, PeerTurns: map[string]*PeerTurn{}}
	const occurrences = 137
	for sequence := 1; sequence <= occurrences; sequence++ {
		instruction, err := (event.Draft{
			Kind: event.UserInstruction, Actor: "user",
			Data: event.UserInstructionData{Text: fmt.Sprintf("instruction-%03d", sequence)},
		}).Materialize(state.SessionID, int64(sequence), time.Unix(int64(sequence), 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(instruction); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.UserInstructions) != occurrences || state.UserInstructionsArchived != 0 {
		t.Fatalf("projection erased evidence before model budgeting: visible=%d archived=%d",
			len(state.UserInstructions), state.UserInstructionsArchived)
	}
	_, view := activeAgentMessages(visionAssistedCodingTeam(), visionAssistedCodingTeam().Primary, state, nil, false)
	if !strings.Contains(view, "instruction-001") || !strings.Contains(view, "instruction-137") {
		t.Fatal("structured projection applied a count-based recent-tail policy before gateway budgeting")
	}
}

func TestPrimaryPromptHasConversationAndMovableLeadershipContract(t *testing.T) {
	p := visionAssistedCodingTeam()
	state := &Projection{
		Task:          "Fix the compiler error shown in the screenshot.",
		TaskInput:     content.Text("Fix the compiler error shown in the screenshot."),
		TaskReference: "alt://context/records/current",
		LeaderID:      p.Primary.ID,
		ConversationHistory: []ConversationTurn{{
			SessionID: "earlier", Task: "Create the parser.", Answer: "Implemented the parser.",
			Status: "completed", LeaderID: p.Primary.ID,
		}},
		Delegations: map[string]*Delegation{}, PeerTurns: map[string]*PeerTurn{},
	}
	system, view := activeAgentMessages(p, p.Primary, state, nil, true)
	for _, expected := range []string{
		"Exactly one agent may lead", "handoff_leadership", "next user turn", p.Primary.Definition,
	} {
		if !strings.Contains(system, expected) {
			t.Fatalf("active-agent contract is missing %q:\n%s", expected, system)
		}
	}
	if strings.Contains(system, `"kind": "coordinate"`) {
		t.Fatal("tool-capable base prompt duplicates the tool-call schema as fallback JSON")
	}
	for _, expected := range []string{
		`"primary":"deepseek-coder"`, `"id":"research-peer"`,
		`"id":"vision-specialist"`,
	} {
		if !strings.Contains(system, expected) {
			t.Fatalf("stable Team prompt is missing %q:\n%s", expected, system)
		}
	}
	for _, expected := range []string{"Create the parser.", "Implemented the parser."} {
		if !strings.Contains(view, expected) {
			t.Fatalf("working view is missing %q:\n%s", expected, view)
		}
	}
	for _, repeated := range []string{"authorized_peers", "authorized_specialists", "inherited_runtime_tools"} {
		if strings.Contains(view, repeated) {
			t.Fatalf("runtime snapshot repeats stable field %q: %s", repeated, view)
		}
	}
}

func TestUnsupportedToolCallingFallbackCarriesCompleteCoordinationContract(t *testing.T) {
	for _, expected := range []string{
		`"kind": "coordinate"`, `"specialist_id"`, `"peer_id"`,
		`"attachments"`, `"depends_on"`, `"handoff"`,
	} {
		if !strings.Contains(coordinationFallbackInstruction, expected) {
			t.Fatalf("coordination fallback is missing %q", expected)
		}
	}
}

func TestHandoffRecipientGetsTheSameExactUserInputAPI(t *testing.T) {
	p := visionAssistedCodingTeam()
	state := &Projection{
		Task: "exact user words", TaskInput: content.Text("exact user words"),
		LeaderID: "research-peer", Delegations: map[string]*Delegation{}, PeerTurns: map[string]*PeerTurn{},
	}
	system, _ := activeAgentMessages(p, p.Peers[0], state, nil, true)
	if !strings.Contains(system, "durable, context-bearing ALT Team agent") ||
		!strings.Contains(system, "When you hold leadership") {
		t.Fatal("handoff recipient did not receive the shared leadership-capable agent contract")
	}
	if !strings.Contains(system, p.Peers[0].Definition) {
		t.Fatal("handoff recipient lost its user-authored role")
	}
}

func TestSpecialistPromptIsThoroughlyStatelessAndCallerComplete(t *testing.T) {
	p := visionAssistedCodingTeam()
	specialist := p.Specialists[0]
	delegation := &Delegation{Spec: event.DelegationSpec{
		ID: "vision-call-1", CallerID: p.Primary.ID, SpecialistID: specialist.ID,
		Objective: "Transcribe the exact compiler diagnostic visible in image-1.",
		Context:   "The caller needs filenames, line numbers, and the complete error text.",
	}}
	system, user := specialistMessages(p, specialist, delegation)
	for _, expected := range []string{"thoroughly stateless", "conversation history", "cannot consult", specialist.Definition} {
		if !strings.Contains(system, expected) {
			t.Fatalf("specialist contract is missing %q:\n%s", expected, system)
		}
	}
	for _, expected := range []string{delegation.Spec.Objective, delegation.Spec.Context} {
		if !strings.Contains(user, expected) {
			t.Fatalf("caller-authored specialist prompt is missing %q: %s", expected, user)
		}
	}
	for _, forbidden := range []string{"conversation_history", "team_evidence", "prior_rounds", "current_user_task"} {
		if strings.Contains(user, forbidden) {
			t.Fatalf("specialist received implicit state field %q: %s", forbidden, user)
		}
	}
}

func TestRepeatedSpecialistCallsDoNotAccumulatePromptMemory(t *testing.T) {
	p := visionAssistedCodingTeam()
	specialist := p.Specialists[0]
	first := &Delegation{Spec: event.DelegationSpec{ID: "first", SpecialistID: specialist.ID, Objective: "Read image-1."}}
	second := &Delegation{Spec: event.DelegationSpec{ID: "second", SpecialistID: specialist.ID, Objective: "Read image-1."}}
	firstSystem, firstUser := specialistMessages(p, specialist, first)
	secondSystem, secondUser := specialistMessages(p, specialist, second)
	if firstSystem != secondSystem || firstUser != secondUser {
		t.Fatal("specialist prompt depends on invocation identity or prior invocation")
	}
}

func TestPeerConsultationReceivesBroadDurableStateButNotLeadership(t *testing.T) {
	p := visionAssistedCodingTeam()
	state := &Projection{
		Task:          "Decide whether the parser design needs external evidence.",
		TaskReference: "alt://context/records/task", LeaderID: p.Primary.ID,
		ConversationHistory: []ConversationTurn{{Task: "Implement parsing", Answer: "Parser added", Status: "completed"}},
		Delegations: map[string]*Delegation{
			"visual": {Spec: event.DelegationSpec{ID: "visual", SpecialistID: "vision-specialist", Objective: "Read the diagnostic"}, Status: DelegationCompleted, Result: "undefined symbol at parser.go:42"},
		},
		PeerTurns: map[string]*PeerTurn{},
	}
	turn := &PeerTurn{Spec: event.PeerTurnSpec{
		ID: "peer-1", CallerID: p.Primary.ID, PeerID: p.Peers[0].ID,
		CollaborationID: "research-thread", Round: 1,
		Objective: "Assess whether the proposed fix matches the language specification.",
	}}
	system, user := peerMessages(p, p.Peers[0], turn, nil, state, true)
	if !strings.Contains(system, "peer consultation") || !strings.Contains(user, `"holds_leadership":false`) {
		t.Fatal("consulted peer was allowed to behave as leader")
	}
	if !strings.Contains(system, `{"result":"concise but complete contribution"`) || strings.Contains(user, "response_contract") {
		t.Fatal("peer response contract was not kept once in the stable prompt")
	}
	for _, expected := range []string{"Implement parsing", "Parser added", "undefined symbol", turn.Spec.Objective} {
		if !strings.Contains(user, expected) {
			t.Fatalf("peer durable view is missing %q:\n%s", expected, user)
		}
	}
}

func TestRepeatedAgentSnapshotOmitsAlreadyDeliveredConversationHistory(t *testing.T) {
	p := visionAssistedCodingTeam()
	state := &Projection{
		Task: "current task", TaskInput: content.Text("current task"), LeaderID: p.Primary.ID,
		ConversationHistory: []ConversationTurn{{Task: "earlier task", Answer: "earlier answer"}},
		UserInstructions:    []string{"new steering instruction"},
		Delegations:         map[string]*Delegation{}, PeerTurns: map[string]*PeerTurn{},
	}
	_, view := activeAgentMessages(p, p.Primary, state, nil, false)
	if strings.Contains(view, "conversation_history") || strings.Contains(view, "earlier answer") {
		t.Fatalf("same-turn snapshot repeated already delivered conversation history: %s", view)
	}
	if !strings.Contains(view, "new steering instruction") {
		t.Fatalf("same-turn snapshot dropped steerable user instructions: %s", view)
	}
}

func visionAssistedCodingTeam() profile.Profile {
	return profile.Profile{
		Schema: profile.CurrentSchema, ID: "vision-assisted-coding", Revision: 1,
		Name: "Vision-assisted coding", Gateway: "opencode",
		Models: map[string]profile.Model{
			"deepseek": {Route: "zen", Name: "deepseek-code"},
			"research": {Route: "zen", Name: "research-model"},
			"vision":   {Route: "zen", Name: "vision-model"},
		},
		Primary: profile.AgentAssignment{
			ID: "deepseek-coder", Model: "deepseek",
			Definition: "Own implementation end-to-end. Because this model is text-only, call the vision specialist whenever pixels carry required evidence.",
			Peers:      []string{"research-peer"}, Specialists: []string{"vision-specialist"},
		},
		Peers: []profile.AgentAssignment{{
			ID: "research-peer", Model: "research",
			Definition: "Own evidence audits whose deliverable is a sourced conclusion; otherwise contribute as a peer.",
		}},
		Specialists: []profile.SpecialistAssignment{{
			ID: "vision-specialist", Model: "vision",
			Definition: "Inspect only explicitly attached images and return exact observable visual evidence to the caller.",
		}},
	}
}
