package orchestrator

import "testing"

func TestCoordinateDecisionUsesExplicitWireDiscriminator(t *testing.T) {
	decision, err := decodeJSONObject[AgentDecision](`{
  "kind":"coordinate",
  "assessment":"The blind coding primary needs the image transcribed.",
  "delegations":[{"key":"inspect-screenshot","specialist_id":"vision","objective":"Read every visible compiler error from the screenshot.","attachments":["image-1"]}],
  "peer_turns":[],"cancel":[],"handoff":null
}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAgentDecision(decision, 0); err != nil {
		t.Fatal(err)
	}
	if decision.Delegations[0].SpecialistID != "vision" {
		t.Fatalf("unexpected specialist: %#v", decision.Delegations[0])
	}
}

func TestNormalUserRequestedJSONIsNotMistakenForCoordination(t *testing.T) {
	for _, raw := range []string{
		`{"status":"fixed","files":["main.go"]}`,
		`{"kind":"report","assessment":"healthy"}`,
		`{"assessment":"healthy","delegations":[]}`,
	} {
		if looksLikeCoordination(raw) {
			t.Fatalf("ordinary answer was classified as orchestration: %s", raw)
		}
	}
}

func TestHandoffMustBeExclusive(t *testing.T) {
	decision := AgentDecision{
		Kind: "coordinate", Assessment: "The research peer should own this evidence audit.",
		Handoff:     &ProposedHandoff{PeerID: "research", Reason: "Evidence is the deliverable."},
		Delegations: []ProposedDelegation{{Key: "also-call", SpecialistID: "vision", Objective: "This makes the transition ambiguous."}},
	}
	if err := validateAgentDecision(decision, 0); err == nil {
		t.Fatal("handoff plus parallel work was accepted")
	}
}

func TestIdleCoordinationWithoutExistingWorkIsRejected(t *testing.T) {
	decision := AgentDecision{Kind: "coordinate", Assessment: "Wait."}
	if err := validateAgentDecision(decision, 0); err == nil {
		t.Fatal("empty coordination cycle was accepted")
	}
	if err := validateAgentDecision(decision, 1); err != nil {
		t.Fatalf("waiting on existing work was rejected: %v", err)
	}
}
