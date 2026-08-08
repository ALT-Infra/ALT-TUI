package nativegui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"altv1/internal/application"
	"altv1/internal/profile"
	"altv1/internal/provider"

	"github.com/cloudwego/eino/components/model"
)

type fixedCatalogGateway struct{ id string }

func (g fixedCatalogGateway) Descriptor() provider.GatewayDescriptor {
	return provider.GatewayDescriptor{
		ID: g.id, Name: strings.ToUpper(g.id), CredentialEnvironment: strings.ToUpper(g.id) + "_KEY",
		Routes: []provider.GatewayRoute{{ID: "models", Label: "Models"}}, MultiModelCatalog: true,
	}
}

func (g fixedCatalogGateway) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return []provider.CatalogModel{{Gateway: g.id, Route: "models", ID: g.id + "/one"}}, nil
}

func (fixedCatalogGateway) NewChatModel(context.Context, profile.Model, provider.Mode) (model.BaseChatModel, error) {
	return nil, nil
}

func TestSizedExchangePublishesOnceAcrossProbeAndTransfer(t *testing.T) {
	ctx := context.Background()
	app, err := application.OpenAt(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	choice := func(id string) ModelChoice {
		return ModelChoice{Route: "zen", ID: id}
	}
	catalog := []provider.CatalogModel{
		{Gateway: "opencode", Route: "zen", ID: "router-model"},
		{Gateway: "opencode", Route: "zen", ID: "engineering-model"},
		{Gateway: "opencode", Route: "zen", ID: "research-model"},
	}
	host := &Host{
		ctx: ctx, app: app, launch: Launch{Mode: ModeTeam}, teamView: TeamViewNew, catalog: catalog,
	}
	request, err := json.Marshal(Request{
		Operation: "team.publish",
		Draft: &TeamDraft{
			ID: "sized-exchange", Name: "Sized exchange", Gateway: "opencode",
			Router: DraftAssignment{
				Model: choice("router-model"), Definition: "Choose the responsible Lead.",
			},
			Members: []DraftMember{
				{
					ID: "engineering", Model: choice("engineering-model"),
					Definition: "Own implementation work.",
				},
				{
					ID: "research", Model: choice("research-model"),
					Definition: "Own source-driven investigation.",
				},
			},
			RouterEdges: []string{"engineering", "research"},
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

func TestGatewaySelectionPreservesDraftAndReplacesTheWholeCatalog(t *testing.T) {
	ctx := context.Background()
	app, err := application.OpenAt(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	registry := provider.NewRegistry()
	for _, id := range []string{"alpha", "beta"} {
		if err := registry.Register(fixedCatalogGateway{id: id}); err != nil {
			t.Fatal(err)
		}
	}
	app.Providers = registry
	draft := NewDraft()
	draft.Name = "Work retained before choosing an account"
	draft.Members = []DraftMember{{ID: "lead", Model: ModelChoice{Route: "old", ID: "old/model"}}}
	draft.Router.Model = ModelChoice{Route: "old", ID: "old/router"}
	host := &Host{ctx: ctx, app: app, launch: Launch{Mode: ModeTeam}, teamView: TeamViewNew, draft: &draft}

	response := host.exchange(Request{Operation: "team.gateway", Gateway: "alpha", Draft: &draft})
	if !response.OK || response.Initial == nil || response.Initial.Draft == nil {
		t.Fatalf("select alpha: %#v", response)
	}
	selected := response.Initial.Draft
	if selected.Name != draft.Name || selected.Gateway != "alpha" {
		t.Fatalf("gateway selection lost the local draft: %#v", selected)
	}
	if selected.Router.Model.ID != "" || selected.Members[0].Model.ID != "" {
		t.Fatalf("old-gateway models survived selection: %#v", selected)
	}
	if len(response.Initial.Catalog) != 1 || response.Initial.Catalog[0].Gateway != "alpha" {
		t.Fatalf("catalog was not atomically scoped to alpha: %#v", response.Initial.Catalog)
	}

	selected.Router.Model = ModelChoice{Route: "models", ID: "alpha/router"}
	selected.Members[0].Model = ModelChoice{Route: "models", ID: "alpha/lead"}
	response = host.exchange(Request{Operation: "team.gateway", Gateway: "beta", Draft: selected})
	if !response.OK || response.Initial == nil || response.Initial.Draft.Gateway != "beta" {
		t.Fatalf("select beta: %#v", response)
	}
	if response.Initial.Draft.Router.Model.ID != "" || response.Initial.Draft.Members[0].Model.ID != "" {
		t.Fatal("changing the Team account did not invalidate every catalog selection")
	}
	if len(response.Initial.Catalog) != 1 || response.Initial.Catalog[0].Gateway != "beta" {
		t.Fatalf("catalog contains another gateway: %#v", response.Initial.Catalog)
	}
}
