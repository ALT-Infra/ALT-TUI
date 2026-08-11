package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	builtinprofiles "altv1/profiles"
)

func TestConversationHistoryReachesRouterLeadAndFinalPrompts(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	history := []ConversationTurn{{
		Task:   "Design the persistence boundary.",
		Answer: "Use SQLite WAL with ordered events.",
		Status: "completed",
		LeadID: "engineering-lead",
		ObservableTrace: []ConversationTrace{{
			Sequence: 7, Kind: event.DelegationCompleted, Actor: "research-lead",
			CorrelationID: "delegation-7",
			Data:          json.RawMessage(`{"result":"WAL recovery was directly observed."}`),
		}},
	}}
	_, router := routerMessages(document.Profile, "Now test its recovery behavior.", history)
	state := &Projection{
		Task:                "Now test its recovery behavior.",
		ConversationHistory: history,
		Delegations:         make(map[string]*Delegation),
	}
	lead := document.Profile.Leads[0]
	_, leadPrompt := leadMessages(document.Profile, lead, state, nil)
	_, finalPrompt := finalMessages(document.Profile, lead, state, "Answer the follow-up.")

	for name, prompt := range map[string]string{
		"router": router,
		"lead":   leadPrompt,
		"final":  finalPrompt,
	} {
		for _, expected := range []string{
			"Design the persistence boundary.",
			"Use SQLite WAL with ordered events.",
			"Now test its recovery behavior.",
			"delegation.completed",
			"WAL recovery was directly observed.",
		} {
			if !strings.Contains(prompt, expected) {
				t.Fatalf("%s prompt is missing conversation context %q:\n%s", name, expected, prompt)
			}
		}
	}
}

func TestFinalSynthesisReceivesCurrentLeadToolEvidenceWithoutRediscovery(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	state := &Projection{
		Task:        "Check one immutable reference.",
		Delegations: make(map[string]*Delegation), PeerTurns: make(map[string]*PeerTurn),
		ObservableTrace: []ConversationTrace{
			{Reference: "alt://context/records/00000000-0000-7000-8000-000000000011", Sequence: 11, Kind: event.ToolCalled, Actor: "engineering-lead", Data: json.RawMessage(`{"tool":"context_open","arguments":"known-reference"}`)},
			{Reference: "alt://context/records/00000000-0000-7000-8000-000000000012", Sequence: 12, Kind: event.ToolCompleted, Actor: "engineering-lead", Data: json.RawMessage(`{"tool":"context_open","failed":true,"error":"context record not found"}`)},
		},
	}
	system, user := finalMessages(document.Profile, document.Profile.Leads[0], state, "Report the observed access result.")
	for _, expected := range []string{
		"completed immutable call", "current_tool_evidence",
		"context_open", "context record not found",
		"alt://context/records/00000000-0000-7000-8000-000000000012",
	} {
		if !strings.Contains(system+user, expected) {
			t.Fatalf("final synthesis omitted delivered tool evidence %q:\n%s\n%s", expected, system, user)
		}
	}
	leadSystem, leadUser := leadMessages(document.Profile, document.Profile.Leads[0], state, nil)
	for _, expected := range []string{"completed immutable call", "current_tool_evidence", "context record not found"} {
		if !strings.Contains(leadSystem+leadUser, expected) {
			t.Fatalf("next Lead turn omitted delivered tool evidence %q:\n%s\n%s", expected, leadSystem, leadUser)
		}
	}
}

func TestLeadWorkingViewIsBoundedAndKeepsExactRecallPaths(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	lead := document.Profile.Leads[0]
	state := &Projection{
		Task:          "Preserve the whole request while bounding old evidence.",
		TaskReference: "alt://context/records/00000000-0000-7000-8000-000000000001",
		Delegations:   make(map[string]*Delegation), PeerTurns: make(map[string]*PeerTurn),
	}
	for index := 0; index < 200; index++ {
		id := fmt.Sprintf("%04d", index)
		marker := fmt.Sprintf("exact-result-marker-%04d", index)
		state.Delegations[id] = &Delegation{
			Spec:   event.DelegationSpec{ID: id, MemberID: "research", Objective: "Investigate " + marker},
			Status: DelegationCompleted, Result: marker,
			SpecSequence: int64(index*2 + 1), ResultSequence: int64(index*2 + 2),
			SpecReference:   fmt.Sprintf("alt://context/records/00000000-0000-7000-8001-%012d", index*2+1),
			ResultReference: fmt.Sprintf("alt://context/records/00000000-0000-7000-8001-%012d", index*2+2),
		}
	}
	_, prompt := leadMessages(document.Profile, lead, state, nil)
	if strings.Contains(prompt, "exact-result-marker-0000") {
		t.Fatal("old exact evidence leaked back into the bounded working view")
	}
	if !strings.Contains(prompt, "exact-result-marker-0199") ||
		!strings.Contains(prompt, `"omitted_entries": 188`) ||
		!strings.Contains(prompt, "context_search") || !strings.Contains(prompt, "context_open") {
		t.Fatalf("working view omitted its recent evidence or recall contract:\n%s", prompt)
	}
	if len(prompt) > 35_000 {
		t.Fatalf("working view grew with archived evidence: %d bytes", len(prompt))
	}
	_, repeated := leadMessages(document.Profile, lead, state, nil)
	if prompt != repeated {
		t.Fatal("the same durable state produced a non-deterministic working view")
	}
}

func TestLargeRecentEvidenceUsesUTF8SafePreviewAndExactReference(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	lead := document.Profile.Leads[0]
	reference := "alt://context/records/00000000-0000-7000-8000-000000000099"
	state := &Projection{Task: "Read the result", Delegations: map[string]*Delegation{
		"d": {
			Spec:   event.DelegationSpec{ID: "d", MemberID: "research", Objective: "Inspect Unicode evidence"},
			Status: DelegationCompleted, Result: strings.Repeat("عِلْم🧭", 5_000),
			ResultReference: reference,
		},
	}, PeerTurns: make(map[string]*PeerTurn)}
	_, prompt := leadMessages(document.Profile, lead, state, nil)
	if !strings.Contains(prompt, reference) || !strings.Contains(prompt, `"compacted": true`) || !json.Valid([]byte(strings.TrimPrefix(prompt, "CURRENT SESSION STATE:\n"))) {
		t.Fatalf("large evidence did not retain a valid bounded exact reference:\n%s", prompt)
	}
}

func TestSteeringHistoryIsBoundedAndEveryVisibleInstructionKeepsItsExactReference(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	state := &Projection{
		Task: "Long-running work", Delegations: make(map[string]*Delegation), PeerTurns: make(map[string]*PeerTurn),
	}
	for index := 0; index < 100; index++ {
		state.UserInstructions = append(state.UserInstructions, fmt.Sprintf("instruction-%03d", index))
		state.UserInstructionReferences = append(state.UserInstructionReferences,
			fmt.Sprintf("alt://context/records/00000000-0000-7000-8002-%012d", index))
	}
	_, prompt := leadMessages(document.Profile, document.Profile.Leads[0], state, nil)
	if strings.Contains(prompt, "instruction-000") || !strings.Contains(prompt, "instruction-099") ||
		!strings.Contains(prompt, `"archived_occurrences": 88`) ||
		!strings.Contains(prompt, "00000000-0000-7000-8002-000000000099") {
		t.Fatalf("instruction view was not bounded and referenced:\n%s", prompt)
	}
}

func TestProjectionBoundsToolTraceMemoryWhileCanonicalEventsRemainExternal(t *testing.T) {
	state := &Projection{SessionID: "session", Delegations: make(map[string]*Delegation), PeerTurns: make(map[string]*PeerTurn)}
	for sequence := int64(1); sequence <= 100; sequence++ {
		item, err := (event.Draft{
			Kind: event.ToolCompleted, Actor: "engineering",
			Data: event.ToolCompletedData{ToolCallID: fmt.Sprintf("call-%d", sequence), Tool: "context_open", Result: "exact result"},
		}).Materialize("session", sequence, time.Unix(sequence, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(item); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.ObservableTrace) != projectionTraceLimit || state.ObservableTraceArchived != 100-projectionTraceLimit {
		t.Fatalf("bounded trace = %d recent + %d archived", len(state.ObservableTrace), state.ObservableTraceArchived)
	}
	if state.ObservableTrace[0].Sequence != int64(100-projectionTraceLimit+1) {
		t.Fatalf("oldest retained trace sequence = %d", state.ObservableTrace[0].Sequence)
	}
}

func TestCalledMemberReceivesOnlyDefinitionObjectiveContextAndToolDiscoveryPolicy(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	p := document.Profile
	lead := p.Leads[0]
	member := p.CallableMembersFor(lead)[0]
	delegation := &Delegation{Spec: event.DelegationSpec{
		ID: "d2", MemberID: member.ID,
		Objective: "Check the bounded endpoint behavior.",
		Context:   "The Lead explicitly curated this evidence.",
		DependsOn: []string{"secret-dependency-id"},
	}}
	system, user := memberMessages(p, member, delegation)

	if !strings.Contains(system, member.Definition) ||
		!strings.Contains(system, "tool_search") {
		t.Fatal("stable definition or dynamic tool-discovery policy is absent")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(user), &payload); err != nil {
		t.Fatalf("called-member payload is not JSON: %v\n%s", err, user)
	}
	if len(payload) != 2 ||
		payload["objective"] != delegation.Spec.Objective ||
		payload["context"] != delegation.Spec.Context {
		t.Fatalf("called-member payload crossed its context boundary: %#v", payload)
	}
	for _, forbidden := range []string{
		"user_task", "conversation_history", "dependency_results",
		"selected_lead", "secret-dependency-id",
	} {
		if strings.Contains(user, forbidden) {
			t.Fatalf("called-member prompt leaked %q:\n%s", forbidden, user)
		}
	}
}

func TestCurrentPromptsDescribeWorkWithoutAssigningPersonas(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	p := document.Profile
	lead := p.Leads[0]
	member := p.CallableMembersFor(lead)[0]
	state := &Projection{
		Task:        "Compare the current framework support before implementing.",
		Delegations: make(map[string]*Delegation),
	}
	delegation := &Delegation{Spec: event.DelegationSpec{
		ID: "d1", MemberID: member.ID,
		Objective: "Establish whether the framework supports this requirement.",
	}}

	routerSystem, _ := routerMessages(p, state.Task, nil)
	leadSystem, _ := leadMessages(p, lead, state, nil)
	memberSystem, _ := memberMessages(p, member, delegation)
	finalSystem, _ := finalMessages(p, lead, state, "Answer from the available evidence.")

	for name, prompt := range map[string]string{
		"router": routerSystem, "lead": leadSystem,
		"member": memberSystem, "final": finalSystem,
	} {
		lower := strings.ToLower(prompt)
		for _, forbidden := range []string{"you are", "act as", "\npersona:"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s prompt contains persona framing %q:\n%s", name, forbidden, prompt)
			}
		}
	}
	if !strings.Contains(leadSystem, lead.Definition) {
		t.Fatal("Lead definition is not passed through verbatim")
	}
	if !strings.Contains(memberSystem, member.Definition) {
		t.Fatal("member definition is not passed through verbatim")
	}
}

func TestRouterContractClassifiesTheDeliverableRatherThanQuestionGrammar(t *testing.T) {
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	system, _ := routerMessages(
		document.Profile,
		"How does one write a Hello World program in Brainfuck?",
		nil,
	)
	for _, required := range []string{
		"work and deliverable needed",
		"not by whether its sentence is phrased as a question",
		"wording used to request some other",
	} {
		if !strings.Contains(system, required) {
			t.Fatalf("Router contract is missing %q:\n%s", required, system)
		}
	}
}
