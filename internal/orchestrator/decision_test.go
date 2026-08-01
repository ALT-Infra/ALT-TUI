package orchestrator

import (
	"strings"
	"testing"

	"altv1/internal/profile"
)

func TestRouterDecisionRequiresTheDeclaredWireContract(t *testing.T) {
	decision, err := decodeJSONObject[RouterDecision](
		`{"lead_id":"engineering-lead","confidence":0.9,"basis":"Engineering owns the deliverable."}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Basis != "Engineering owns the deliverable." {
		t.Fatalf("basis = %q", decision.Basis)
	}
	for _, invalid := range []string{
		`{"lead_id":"engineering-lead","confidence":0.9,"decision_basis":"alias"}`,
		`explanation before {"lead_id":"engineering-lead","confidence":0.9,"basis":"embedded"} explanation after`,
	} {
		if _, err := decodeJSONObject[RouterDecision](invalid); err == nil {
			t.Fatalf("undeclared structured-output variant was accepted: %q", invalid)
		}
	}
}

func TestRouterPromptDeclaresTheExactWireFields(t *testing.T) {
	t.Parallel()

	system, _ := routerMessages(profile.Profile{}, "task", nil)
	for _, field := range []string{`"lead_id"`, `"confidence"`, `"basis"`} {
		if !strings.Contains(system, field) {
			t.Errorf("router system prompt does not declare %s", field)
		}
	}
}

func TestEnvelopedJSONRecoveryIsExactAndUnambiguous(t *testing.T) {
	t.Parallel()

	value, err := decodeEnvelopedJSONObject[RouterDecision](
		"Here is the requested object:\n```json\n" +
			`{"lead_id":"engineering","confidence":1,"basis":"Code is the deliverable."}` +
			"\n```",
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.LeadID != "engineering" || value.Basis != "Code is the deliverable." {
		t.Fatalf("recovered decision = %#v", value)
	}

	for _, invalid := range []string{
		`{"lead_id":"a","confidence":1,"basis":"one"} {"lead_id":"b","confidence":1,"basis":"two"}`,
		`prose without a JSON object`,
		`prefix {"lead_id":"a","confidence":1,"basis":"unterminated"`,
		`prefix {"lead_id":"a","confidence":1,"basis":"ok","invented":true} suffix`,
	} {
		if _, err := decodeEnvelopedJSONObject[RouterDecision](invalid); err == nil {
			t.Fatalf("ambiguous or invalid envelope was accepted: %q", invalid)
		}
	}
}
