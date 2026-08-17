package nativegui

import (
	"strings"
	"testing"

	"altv1/internal/profile"
	"altv1/internal/provider"
)

func TestDraftRoundTripPreservesPrimaryPeersSpecialistsAndDefinitions(t *testing.T) {
	catalog := meaningfulCatalog()
	draft := meaningfulDraft()
	value := draft.Profile()
	if value.Primary.ID != "deepseek-coder" || len(value.Peers) != 1 || len(value.Specialists) != 1 {
		t.Fatalf("profile roles = %#v", value)
	}
	if peer, ok := value.PeerAgentFor(value.Primary, "research-peer"); !ok || peer.ID != "research-peer" {
		t.Fatal("peer edge did not survive draft conversion")
	}
	if specialist, ok := value.SpecialistFor(value.Primary, "vision-specialist"); !ok || specialist.ID != "vision-specialist" {
		t.Fatal("directed specialist edge did not survive draft conversion")
	}
	roundTrip := DraftFromProfile(value, catalog)
	if roundTrip.Primary.Definition != draft.Primary.Definition || roundTrip.Peers[0].Definition != draft.Peers[0].Definition || roundTrip.Specialists[0].Definition != draft.Specialists[0].Definition {
		t.Fatalf("verbatim role definitions changed: %#v", roundTrip)
	}
	if len(roundTrip.PeerEdges) != 1 || len(roundTrip.SpecialistEdges) != 1 {
		t.Fatalf("graph edges changed: %#v", roundTrip)
	}
}

func TestPrimaryOnlyTeamIsValidAndSpecialistsAreOptional(t *testing.T) {
	draft := TeamDraft{
		ID: "solo-coder", Name: "Solo coder", Gateway: "opencode",
		Primary: DraftMember{
			ID: "deepseek-coder", Model: ModelChoice{Route: "zen", ID: "deepseek-code"},
			Definition: "Own code implementation and verification end-to-end.",
		},
	}
	diagnostics := DiagnosticsForDraft(draft, meaningfulCatalog())
	if hasErrors(diagnostics) {
		t.Fatalf("primary-only Team was rejected: %#v", diagnostics)
	}
}

func TestDraftRejectsInvalidRoleEdgesAndMissingCatalogSelections(t *testing.T) {
	draft := meaningfulDraft()
	draft.PeerEdges = append(draft.PeerEdges, DraftPeerEdge{FirstAgentID: "deepseek-coder", SecondAgentID: "vision-specialist"})
	draft.SpecialistEdges = append(draft.SpecialistEdges, DraftSpecialistEdge{AgentID: "vision-specialist", SpecialistID: "research-peer"})
	draft.Specialists[0].Model = ModelChoice{Route: "zen", ID: "removed-model"}
	diagnostics := DiagnosticsForDraft(draft, meaningfulCatalog())
	for _, fragment := range []string{"leadership-capable agents", "caller must be a leadership-capable agent", "callee must be a stateless specialist", "not in the current authenticated gateway catalog"} {
		if !diagnosticContains(diagnostics, fragment) {
			t.Fatalf("missing diagnostic %q: %#v", fragment, diagnostics)
		}
	}
}

func TestSharedSpecialistEdgesRemainDirectedAndValid(t *testing.T) {
	draft := meaningfulDraft()
	draft.SpecialistEdges = append(draft.SpecialistEdges, DraftSpecialistEdge{AgentID: "research-peer", SpecialistID: "vision-specialist"})
	diagnostics := DiagnosticsForDraft(draft, meaningfulCatalog())
	if hasErrors(diagnostics) {
		t.Fatalf("shared specialist was rejected: %#v", diagnostics)
	}
	value := draft.Profile()
	if _, ok := value.SpecialistFor(value.Primary, "vision-specialist"); !ok {
		t.Fatal("primary lost shared specialist permission")
	}
	if _, ok := value.SpecialistFor(value.Peers[0], "vision-specialist"); !ok {
		t.Fatal("peer lost shared specialist permission")
	}
}

func meaningfulDraft() TeamDraft {
	return TeamDraft{
		ID: "vision-assisted-coding", Name: "Vision-assisted coding", Gateway: "opencode",
		Primary: DraftMember{
			ID: "deepseek-coder", Model: ModelChoice{Route: "zen", ID: "deepseek-code"},
			Definition: "Own implementation end-to-end; call the vision specialist when pixels carry required evidence.",
		},
		Peers: []DraftMember{{
			ID: "research-peer", Model: ModelChoice{Route: "zen", ID: "research-model"},
			Definition: "Own evidence audits whose deliverable is a sourced conclusion.",
		}},
		Specialists: []DraftMember{{
			ID: "vision-specialist", Model: ModelChoice{Route: "zen", ID: "vision-model"},
			Definition: "Inspect explicitly supplied images and return observable evidence with no retained state.",
		}},
		PeerEdges:       []DraftPeerEdge{{FirstAgentID: "deepseek-coder", SecondAgentID: "research-peer"}},
		SpecialistEdges: []DraftSpecialistEdge{{AgentID: "deepseek-coder", SpecialistID: "vision-specialist"}},
	}
}

func meaningfulCatalog() []provider.CatalogModel {
	return []provider.CatalogModel{
		{Gateway: "opencode", Route: "zen", ID: "deepseek-code"},
		{Gateway: "opencode", Route: "zen", ID: "research-model"},
		{Gateway: "opencode", Route: "zen", ID: "vision-model"},
	}
}

func diagnosticContains(values []Diagnostic, fragment string) bool {
	for _, item := range values {
		if strings.Contains(item.Message, fragment) {
			return true
		}
	}
	return false
}

func TestDraftProfileUsesCurrentSchema(t *testing.T) {
	if got := meaningfulDraft().Profile().Schema; got != profile.CurrentSchema {
		t.Fatalf("draft schema = %d", got)
	}
}
