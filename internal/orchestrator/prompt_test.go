package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"

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
