package nativegui

import (
	"context"
	"strings"
	"testing"
	"time"

	"altv1/internal/application"
	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"
	"altv1/internal/thinking"
	builtinprofiles "altv1/profiles"
)

func TestDraftRoundTripPreservesDefinitionsAndLeadCallEdges(t *testing.T) {
	catalog := testCatalog()
	routerDefinition := "Route only from the definitions below.\n\nPreserve this spacing."
	leadDefinition := "Own implementation.\nDo not rewrite this user's punctuation:  !!"
	researchDefinition := "Establish external evidence and return sources.\n\tKeep this tab."
	draft := TeamDraft{
		Gateway: "opencode",
		ID:      "exact-team",
		Name:    "Exact Team",
		Router: DraftAssignment{
			Model: choice(catalog[0]), Definition: routerDefinition,
		},
		Members: []DraftMember{
			{
				ID: "engineering", Model: choice(catalog[1]),
				Definition: leadDefinition,
			},
			{
				ID: "research", Model: choice(catalog[2]),
				Definition: researchDefinition,
			},
			{
				ID: "specialist", Model: choice(catalog[3]),
				Definition: "Return bounded specialist findings.",
			},
		},
		RouterEdges: []string{"engineering", "research"},
		CallEdges: []DraftCallEdge{{
			LeadID: "research", MemberID: "specialist",
		}},
	}
	if diagnostics := DiagnosticsForDraft(draft, catalog); hasErrors(diagnostics) {
		t.Fatalf("valid draft diagnostics: %#v", diagnostics)
	}
	value := draft.Profile()
	if value.Router.Definition != routerDefinition {
		t.Fatalf("router definition changed: %q", value.Router.Definition)
	}
	if len(value.Leads) != 2 || len(value.Members) != 1 {
		t.Fatalf("roles were not represented once per assignment: %#v", value)
	}
	if value.Leads[0].Definition != leadDefinition ||
		value.Leads[1].Definition != researchDefinition {
		t.Fatal("member definitions were not preserved verbatim across roles")
	}
	roundTrip := DraftFromProfile(value, catalog)
	if roundTrip.Router.Definition != routerDefinition {
		t.Fatal("router definition changed during edit round trip")
	}
	for _, member := range roundTrip.Members {
		switch member.ID {
		case "engineering":
			if member.Definition != leadDefinition || !contains(roundTrip.RouterEdges, member.ID) {
				t.Fatalf("callable Lead changed: %#v", member)
			}
		case "research":
			if member.Definition != researchDefinition || !contains(roundTrip.RouterEdges, member.ID) {
				t.Fatalf("research member changed: %#v", member)
			}
		}
	}
}

func TestDraftRejectsDisappearedModelWithoutSubstitution(t *testing.T) {
	catalog := testCatalog()
	draft := TeamDraft{
		Gateway: "opencode",
		ID:      "missing-model-team",
		Name:    "Missing Model Team",
		Router: DraftAssignment{
			Model: choice(catalog[0]), Definition: strings.Repeat("router ", 10),
		},
		Members: []DraftMember{
			{
				ID: "first", Model: choice(catalog[1]),
				Definition: strings.Repeat("first ", 10),
			},
			{
				ID: "second",
				Model: ModelChoice{
					Route: "zen", ID: "removed-model",
				},
				Definition: strings.Repeat("second ", 10),
			},
		},
		RouterEdges: []string{"first", "second"},
	}
	diagnostics := DiagnosticsForDraft(draft, catalog)
	if !hasErrors(diagnostics) {
		t.Fatalf("disappeared model was accepted: %#v", diagnostics)
	}
	found := false
	for _, item := range diagnostics {
		found = found || strings.Contains(item.Message, "will not substitute")
	}
	if !found {
		t.Fatalf("missing deterministic no-substitution diagnostic: %#v", diagnostics)
	}
	if draft.Members[1].Model.ID != "removed-model" {
		t.Fatal("draft model was silently changed")
	}
}

func TestEmptyDraftDiagnosticsUseAuthoringSurfacePaths(t *testing.T) {
	draft := NewDraft()
	if !strings.HasPrefix(draft.ID, "team-") {
		t.Fatalf("generated Team ID = %q, want team- UUID", draft.ID)
	}
	if another := NewDraft().ID; another == draft.ID {
		t.Fatalf("separate drafts received the same generated Team ID %q", draft.ID)
	}
	diagnostics := DiagnosticsForDraft(draft, testCatalog())
	for _, item := range diagnostics {
		if strings.HasPrefix(item.Path, "models.") {
			t.Fatalf("internal profile path leaked into empty builder: %#v", diagnostics)
		}
		if item.Path == "team.id" {
			t.Fatalf("product-generated Team ID was rejected: %#v", diagnostics)
		}
	}
	for _, expected := range []string{"team.name", "router.model", "router.definition", "leads"} {
		if !hasPath(diagnostics, expected) {
			t.Fatalf("missing %s diagnostic: %#v", expected, diagnostics)
		}
	}
}

func TestTeamInspectorLoadsExactRevisionAndRejectsBackendMutation(t *testing.T) {
	ctx := context.Background()
	app, err := application.OpenAt(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Store.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}

	host, err := NewHost(ctx, app, Launch{
		Mode:      ModeTeam,
		ProfileID: document.Profile.ID,
		Revision:  document.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	initial := host.exchange(Request{Operation: "init"})
	if !initial.OK || initial.Initial == nil || initial.Initial.Draft == nil {
		t.Fatalf("inspect init failed: %#v", initial)
	}
	if initial.Initial.Mode != ModeTeam || initial.Initial.View != TeamViewInspect {
		t.Fatalf("mode/view = %q/%q, want Team/Inspect", initial.Initial.Mode, initial.Initial.View)
	}
	if initial.Initial.Draft.BaseRevision != document.Profile.Revision {
		t.Fatalf(
			"loaded revision %d, want %d",
			initial.Initial.Draft.BaseRevision,
			document.Profile.Revision,
		)
	}

	for _, operation := range []string{"team.validate", "team.publish"} {
		response := host.exchange(Request{
			Operation: operation,
			Draft:     initial.Initial.Draft,
		})
		if response.OK || response.Error != "team inspection is read-only" {
			t.Fatalf("%s was not rejected as read-only: %#v", operation, response)
		}
	}
	latest, err := app.Store.Profile(ctx, document.Profile.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Profile.Revision != document.Profile.Revision ||
		latest.Digest != document.Digest {
		t.Fatalf("inspection changed the stored profile: %#v", latest)
	}
	if host.Published() != nil {
		t.Fatal("read-only inspection produced a publication result")
	}
}

func TestPushedThinkingEventIsAppliedImmediatelyAndIdempotently(t *testing.T) {
	projection := thinking.New("conversation", profile.Profile{})
	if err := projection.AddTurn(store.Session{
		ID: "turn", ConversationID: "conversation", Task: "exercise live delivery",
		Status: store.SessionRunning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	host := &Host{
		thinking: projection,
		launch:   Launch{Mode: ModeThinking, SessionID: "turn"},
	}
	item, err := (event.Draft{
		Kind: event.SessionCreated, Actor: "user",
		Data: event.SessionCreatedData{Task: "exercise live delivery"},
	}).Materialize("turn", 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.PushEvent(item); err != nil {
		t.Fatal(err)
	}
	if projection.Active.Sequence != 1 || projection.Active.Nodes["router"] == nil {
		t.Fatalf("push did not update projection immediately: %#v", projection)
	}
	if err := host.PushEvent(item); err != nil {
		t.Fatal(err)
	}
	if projection.Active.Sequence != 1 {
		t.Fatal("replayed pushed event was not deduplicated by sequence")
	}
}

func TestPushedThinkingEventRepairsDroppedSequenceFromLedger(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	catalog := testCatalog()
	draft := TeamDraft{
		Gateway: "opencode",
		ID:      "team", Name: "Team",
		Router: DraftAssignment{
			Model: choice(catalog[0]), Definition: strings.Repeat("route ", 10),
		},
		Members: []DraftMember{
			{
				ID: "first", Model: choice(catalog[1]),
				Definition: strings.Repeat("first ", 10),
			},
			{
				ID: "second", Model: choice(catalog[2]),
				Definition: strings.Repeat("second ", 10),
			},
		},
		RouterEdges: []string{"first", "second"},
	}
	value := draft.Profile()
	value.Revision = 1
	document, err := profile.FromValue(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	session, err := ledger.CreateSession(ctx, document, "repair a dropped event", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	third, err := ledger.Append(ctx, session.ID, event.Draft{
		Kind:  event.RouterStarted,
		Actor: "router",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := ledger.Events(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	host := &Host{
		ctx:      ctx,
		app:      &application.Application{Store: ledger},
		launch:   Launch{Mode: ModeThinking, SessionID: session.ID},
		thinking: thinking.New(session.ConversationID, document.Profile),
	}
	if err := host.thinking.AddTurn(*session); err != nil {
		t.Fatal(err)
	}
	if err := host.PushEvent(events[0]); err != nil {
		t.Fatal(err)
	}
	// Deliver sequence 3 while deliberately omitting sequence 2 from the live
	// pipe. The host must replay 2 from SQLite before it accepts 3.
	if err := host.PushEvent(third); err != nil {
		t.Fatal(err)
	}
	if host.thinking.Active.Sequence != 3 {
		t.Fatalf("sequence = %d, want 3", host.thinking.Active.Sequence)
	}
	if got := host.thinking.Active.Nodes["user"].Metadata["team"]; got != "team@1" {
		t.Fatalf("missing reconciled sequence 2 metadata: %q", got)
	}
}

func TestThinkingHostKeepsLaterTurnsInTheSameSessionProjection(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	value := TeamDraft{
		Gateway: "opencode",
		ID:      "team", Name: "Team",
		Router: DraftAssignment{
			Model: choice(testCatalog()[0]), Definition: strings.Repeat("route ", 10),
		},
		Members: []DraftMember{
			{
				ID: "lead", Model: choice(testCatalog()[1]),
				Definition: strings.Repeat("own engineering outcomes ", 4),
			},
			{
				ID: "research", Model: choice(testCatalog()[2]),
				Definition: strings.Repeat("own evidence investigations ", 4),
			},
		},
		RouterEdges: []string{"lead", "research"},
	}.Profile()
	value.Revision = 1
	document, err := profile.FromValue(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	first, err := ledger.CreateSession(ctx, document, "first prompt", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Append(ctx, first.ID, event.Draft{
		Kind: event.FinalCompleted, Data: event.FinalCompletedData{Answer: "first"},
	}); err != nil {
		t.Fatal(err)
	}
	app := &application.Application{Store: ledger}
	host, err := NewHost(ctx, app, Launch{Mode: ModeThinking, SessionID: first.ID})
	if err != nil {
		t.Fatal(err)
	}

	second, err := ledger.CreateContinuation(ctx, first.ID, document, "second prompt")
	if err != nil {
		t.Fatal(err)
	}
	events, err := ledger.Events(ctx, second.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range events {
		if err := host.PushEvent(item); err != nil {
			t.Fatal(err)
		}
	}
	if host.thinking.SessionID != first.ConversationID {
		t.Fatalf("session identity changed to %q", host.thinking.SessionID)
	}
	if host.thinking.ActiveTurnID != second.ID || len(host.thinking.Turns) != 2 {
		t.Fatalf("later turn was not incorporated live: %#v", host.thinking)
	}
	if host.thinking.Turns[0].Status != thinking.Completed ||
		host.thinking.Turns[1].Status != thinking.Running {
		t.Fatalf("turn statuses = %#v", host.thinking.Turns)
	}
}

func testCatalog() []provider.CatalogModel {
	return []provider.CatalogModel{
		{
			Gateway: "opencode", Route: "zen", ID: "big-pickle",
		},
		{
			Gateway: "opencode", Route: "zen", ID: "deepseek-v4-flash-free",
		},
		{
			Gateway: "opencode", Route: "zen", ID: "mimo-v2.5-free",
		},
		{
			Gateway: "opencode", Route: "zen", ID: "specialist-model",
		},
	}
}

func choice(item provider.CatalogModel) ModelChoice {
	return ModelChoice{
		Route: item.Route, ID: item.ID,
	}
}

func hasPath(values []Diagnostic, path string) bool {
	for _, item := range values {
		if item.Path == path {
			return true
		}
	}
	return false
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
