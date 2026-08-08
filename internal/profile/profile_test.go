package profile_test

import (
	"strings"
	"testing"

	"altv1/internal/profile"
	builtinprofiles "altv1/profiles"
)

func TestBuiltInProfileUsesExactCatalogIdentitiesAndExplicitCallEdges(t *testing.T) {
	document := builtIn(t)
	p := document.Profile
	if p.Schema != profile.CurrentSchema {
		t.Fatalf("built-in schema = %d, want %d", p.Schema, profile.CurrentSchema)
	}
	if p.Router.Definition == "" {
		t.Fatal("Router definition is empty")
	}
	if p.Gateway == "" {
		t.Fatal("Team gateway is empty")
	}

	owners := map[string]string{}
	register := func(memberID, alias string) {
		t.Helper()
		selected := p.Models[alias]
		if selected.Route == "" || selected.Name == "" {
			t.Fatalf("%s has incomplete catalog identity: %#v", alias, selected)
		}
		identity := profile.ModelIdentity(selected)
		if owner, exists := owners[identity]; exists && owner != memberID {
			t.Fatalf("catalog model %s is assigned to both %s and %s", selected.Name, owner, memberID)
		}
		owners[identity] = memberID
	}
	register("$router", p.Router.Model)
	for _, lead := range p.Leads {
		register(lead.ID, lead.Model)
		if lead.Definition == "" {
			t.Fatalf("Lead %s definition is empty", lead.ID)
		}
	}
	for _, member := range p.Members {
		register(member.ID, member.Model)
		if member.Definition == "" {
			t.Fatalf("member %s definition is empty", member.ID)
		}
	}

	engineering, _ := p.Lead("engineering-lead")
	research, _ := p.Lead("research-lead")
	if !contains(engineering.Calls, "research-specialist") ||
		!contains(engineering.Peers, "research-specialist") ||
		!contains(research.Calls, "technical-specialist") ||
		!contains(research.Peers, "technical-specialist") {
		t.Fatal("specialist call and peer edges are missing")
	}
}

func TestDifferentMembersCannotShareExactCatalogIdentity(t *testing.T) {
	document := builtIn(t)
	document.Profile.Members = append(document.Profile.Members, profile.MemberAssignment{
		ID: "duplicate", Model: document.Profile.Leads[0].Model,
		Definition: "This definition is structurally valid.",
	})
	diagnostics := profile.Validate(document.Profile)
	if !hasDiagnostic(diagnostics, profile.Error, "one model is one Team member") {
		t.Fatalf("duplicate catalog identity was not rejected: %#v", diagnostics)
	}
}

func TestLeadCannotCallItself(t *testing.T) {
	document := builtIn(t)
	document.Profile.Leads[1].Calls = []string{"research-lead"}
	diagnostics := profile.Validate(document.Profile)
	if !hasDiagnostic(diagnostics, profile.Error, "cannot call itself") {
		t.Fatalf("self-call was not rejected: %#v", diagnostics)
	}
}

func TestLeadCannotBeUsedAsContributorOrPeer(t *testing.T) {
	document := builtIn(t)
	document.Profile.Leads[0].Calls = []string{"research-lead"}
	document.Profile.Leads[0].Peers = []string{"research-lead"}
	diagnostics := profile.Validate(document.Profile)
	if !hasDiagnostic(diagnostics, profile.Error, "exclusive roles") {
		t.Fatalf("Lead/contributor dual role was not rejected: %#v", diagnostics)
	}
}

func TestDefinitionsRoundTripVerbatim(t *testing.T) {
	document := builtIn(t)
	const definition = "Start with the whole request.\n\n## What matters here\n\nFollow the evidence."
	document.Profile.Leads[0].Definition = definition
	roundTrip, err := profile.FromValue(document.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if actual := roundTrip.Profile.Leads[0].Definition; actual != definition {
		t.Fatalf("definition changed:\nwant %q\ngot  %q", definition, actual)
	}
}

func TestOldSchemaAndOldFieldsAreRejected(t *testing.T) {
	source := strings.Replace(string(builtinprofiles.Engineering), "schema: 1", "schema: 4", 1)
	if document, err := profile.Parse([]byte(source)); err != nil {
		t.Fatal(err)
	} else if !profile.HasErrors(profile.Validate(document.Profile)) {
		t.Fatal("removed schema was accepted")
	}
	source = string(builtinprofiles.Engineering) + "\nlegacy_limits: true\n"
	if _, err := profile.Parse([]byte(source)); err == nil {
		t.Fatal("unknown legacy field was accepted")
	}
}

func TestLegacyPerModelGatewayIsLiftedToTeam(t *testing.T) {
	source := strings.Replace(string(builtinprofiles.Engineering), "gateway: opencode\n", "", 1)
	source = strings.ReplaceAll(source, "    route:", "    gateway: OpenCode\n    route:")
	document, err := profile.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if document.Profile.Gateway != "opencode" {
		t.Fatalf("migrated gateway = %q", document.Profile.Gateway)
	}
	for alias, model := range document.Profile.Models {
		if model.LegacyGateway != "" {
			t.Fatalf("legacy gateway survived normalization for %s", alias)
		}
	}
}

func TestLegacyMixedGatewaysCannotBecomeATeam(t *testing.T) {
	source := strings.Replace(string(builtinprofiles.Engineering), "gateway: opencode\n", "", 1)
	source = strings.ReplaceAll(source, "    route:", "    gateway: opencode\n    route:")
	source = strings.Replace(source, "    gateway: opencode\n", "    gateway: cline\n", 1)
	if _, err := profile.Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "multiple") && !strings.Contains(err.Error(), "not Team gateway") {
		t.Fatalf("legacy mixed-gateway profile was accepted: %v", err)
	}
}

func builtIn(t *testing.T) *profile.Document {
	t.Helper()
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := profile.Validate(document.Profile); profile.HasErrors(diagnostics) {
		t.Fatalf("built-in profile has errors: %#v", diagnostics)
	}
	return document
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func hasDiagnostic(values []profile.Diagnostic, severity profile.Severity, fragment string) bool {
	for _, item := range values {
		if item.Severity == severity && strings.Contains(item.Message, fragment) {
			return true
		}
	}
	return false
}
