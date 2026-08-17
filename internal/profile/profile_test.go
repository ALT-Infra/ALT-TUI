package profile_test

import (
	"strings"
	"testing"

	"altv1/internal/profile"
	builtinprofiles "altv1/profiles"
)

func TestBuiltInProfileUsesRouterlessRolesAndUniqueCatalogModels(t *testing.T) {
	p := builtIn(t, builtinprofiles.Engineering).Profile
	if p.Schema != profile.CurrentSchema || p.Primary.ID == "" || p.Gateway == "" {
		t.Fatalf("incomplete built-in Team: %#v", p)
	}
	owners := map[string]string{}
	register := func(memberID, alias, definition string) {
		t.Helper()
		selected := p.Models[alias]
		if selected.Route == "" || selected.Name == "" || definition == "" {
			t.Fatalf("%s has an incomplete assignment", memberID)
		}
		identity := profile.ModelIdentity(selected)
		if owner := owners[identity]; owner != "" && owner != memberID {
			t.Fatalf("catalog model %s is assigned to both %s and %s", selected.Name, owner, memberID)
		}
		owners[identity] = memberID
	}
	for _, agent := range p.Agents() {
		register(agent.ID, agent.Model, agent.Definition)
	}
	for _, specialist := range p.Specialists {
		register(specialist.ID, specialist.Model, specialist.Definition)
	}
}

func TestBundledFreeTeamsContainOnlyExplicitFreeCatalogModels(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source []byte
	}{
		{name: "general", source: builtinprofiles.Free},
		{name: "engineering", source: builtinprofiles.Engineering},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			p := builtIn(t, fixture.source).Profile
			for alias, selected := range p.Models {
				if selected.Route != "zen" || !strings.HasSuffix(selected.Name, "-free") {
					t.Fatalf("model %s is not an explicit OpenCode free-catalog identity: %#v", alias, selected)
				}
			}
		})
	}
}

func TestDifferentAssignmentsCannotShareExactCatalogIdentity(t *testing.T) {
	document := builtIn(t, builtinprofiles.Engineering)
	document.Profile.Specialists = append(document.Profile.Specialists, profile.SpecialistAssignment{
		ID: "duplicate", Model: document.Profile.Primary.Model,
		Definition: "Perform one bounded, thoroughly stateless check.",
	})
	document.Profile.Primary.Specialists = append(document.Profile.Primary.Specialists, "duplicate")
	diagnostics := profile.Validate(document.Profile)
	if !hasDiagnostic(diagnostics, profile.Error, "one model is one Team member") {
		t.Fatalf("duplicate catalog identity was not rejected: %#v", diagnostics)
	}
}

func TestPeerRelationshipIsUndirectedAndLeadershipCapable(t *testing.T) {
	p := builtIn(t, builtinprofiles.Engineering).Profile
	if len(p.Peers) == 0 {
		t.Fatal("fixture needs one purposeful peer")
	}
	peer := p.Peers[0]
	if _, ok := p.PeerAgentFor(p.Primary, peer.ID); !ok {
		t.Fatal("primary cannot reach declared peer")
	}
	if _, ok := p.PeerAgentFor(peer, p.Primary.ID); !ok {
		t.Fatal("peer relationship is not reciprocal")
	}
}

func TestSpecialistCanBeSharedButRemainsOutsidePeerGraph(t *testing.T) {
	p := builtIn(t, builtinprofiles.Engineering).Profile
	shared := p.Specialists[0].ID
	p.Primary.Specialists = appendUnique(p.Primary.Specialists, shared)
	p.Peers[0].Specialists = appendUnique(p.Peers[0].Specialists, shared)
	if diagnostics := profile.Validate(p); profile.HasErrors(diagnostics) {
		t.Fatalf("shared specialist was rejected: %#v", diagnostics)
	}
	if _, ok := p.SpecialistFor(p.Primary, shared); !ok {
		t.Fatal("primary cannot call shared specialist")
	}
	if _, ok := p.SpecialistFor(p.Peers[0], shared); !ok {
		t.Fatal("peer cannot call shared specialist")
	}
	p.Primary.Peers = append(p.Primary.Peers, shared)
	if !hasDiagnostic(profile.Validate(p), profile.Error, "unknown leadership-capable peer") {
		t.Fatal("stateless specialist was admitted to the peer/leadership graph")
	}
}

func TestDefinitionsRoundTripVerbatim(t *testing.T) {
	document := builtIn(t, builtinprofiles.Engineering)
	const definition = "Start with the whole request.\n\n## What matters here\n\nFollow the evidence."
	document.Profile.Primary.Definition = definition
	roundTrip, err := profile.FromValue(document.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if actual := roundTrip.Profile.Primary.Definition; actual != definition {
		t.Fatalf("definition changed:\nwant %q\ngot  %q", definition, actual)
	}
}

func TestOldSchemaAndRouterFieldsAreRejected(t *testing.T) {
	source := strings.Replace(string(builtinprofiles.Engineering), "schema: 2", "schema: 1", 1)
	if document, err := profile.Parse([]byte(source)); err != nil {
		t.Fatal(err)
	} else if !profile.HasErrors(profile.Validate(document.Profile)) {
		t.Fatal("removed schema was accepted")
	}
	source = string(builtinprofiles.Engineering) + "\nrouter: {model: obsolete}\n"
	if _, err := profile.Parse([]byte(source)); err == nil {
		t.Fatal("removed router field was accepted")
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
	if _, err := profile.Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "not Team gateway") {
		t.Fatalf("legacy mixed-gateway profile was accepted: %v", err)
	}
}

func builtIn(t *testing.T, source []byte) *profile.Document {
	t.Helper()
	document, err := profile.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := profile.Validate(document.Profile); profile.HasErrors(diagnostics) {
		t.Fatalf("built-in profile has errors: %#v", diagnostics)
	}
	return document
}

func appendUnique(values []string, expected string) []string {
	for _, value := range values {
		if value == expected {
			return values
		}
	}
	return append(values, expected)
}

func hasDiagnostic(values []profile.Diagnostic, severity profile.Severity, fragment string) bool {
	for _, item := range values {
		if item.Severity == severity && strings.Contains(item.Message, fragment) {
			return true
		}
	}
	return false
}
