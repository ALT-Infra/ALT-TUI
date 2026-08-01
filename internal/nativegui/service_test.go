package nativegui

import (
	"context"
	"encoding/json"
	"testing"

	"altv1/internal/application"
	"altv1/internal/provider"
)

func TestSizedExchangePublishesOnceAcrossProbeAndTransfer(t *testing.T) {
	ctx := context.Background()
	app, err := application.OpenAt(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	choice := func(id string) ModelChoice {
		return ModelChoice{Gateway: "opencode", Route: "zen", ID: id}
	}
	catalog := []provider.CatalogModel{
		{Gateway: "opencode", Route: "zen", ID: "router-model"},
		{Gateway: "opencode", Route: "zen", ID: "engineering-model"},
		{Gateway: "opencode", Route: "zen", ID: "research-model"},
	}
	host := &Host{
		ctx: ctx, app: app, launch: Launch{Mode: ModeTeamNew}, catalog: catalog,
	}
	request, err := json.Marshal(Request{
		Operation: "team.publish",
		Draft: &TeamDraft{
			ID: "sized-exchange", Name: "Sized exchange",
			Router: DraftAssignment{
				Model: choice("router-model"), Definition: "Choose the responsible Lead.",
			},
			Members: []DraftMember{
				{
					ID: "engineering", Model: choice("engineering-model"),
					Definition: "Own implementation work.", Lead: true,
				},
				{
					ID: "research", Model: choice("research-model"),
					Definition: "Own source-driven investigation.", Lead: true,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared := host.ExchangeSized(request, 0)
	transferred := host.ExchangeSized(request, len(prepared))
	if string(prepared) != string(transferred) {
		t.Fatal("probe and transfer returned different response bytes")
	}
	var response Response
	if err := json.Unmarshal(transferred, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Published == nil || response.Published.Revision != 1 {
		t.Fatalf("unexpected publish response: %#v", response)
	}
	profiles, err := app.Store.ListProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Revision != 1 {
		t.Fatalf("sizing handshake executed publication more than once: %#v", profiles)
	}
}
