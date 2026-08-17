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
	return provider.GatewayDescriptor{ID: g.id, Name: strings.ToUpper(g.id), CredentialEnvironment: strings.ToUpper(g.id) + "_KEY", Routes: []provider.GatewayRoute{{ID: "models", Label: "Models"}}, MultiModelCatalog: true}
}

func (g fixedCatalogGateway) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return []provider.CatalogModel{
		{Gateway: g.id, Route: "models", ID: g.id + "/code"},
		{Gateway: g.id, Route: "models", ID: g.id + "/research"},
		{Gateway: g.id, Route: "models", ID: g.id + "/vision"},
	}, nil
}

func (fixedCatalogGateway) NewChatModel(context.Context, profile.Model, provider.Mode) (model.BaseChatModel, error) {
	return nil, nil
}

func TestSizedExchangePublishesRouterlessDraftOnce(t *testing.T) {
	ctx := context.Background()
	app, err := application.OpenAt(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	draft := meaningfulDraft()
	host := &Host{ctx: ctx, app: app, launch: Launch{Mode: ModeTeam}, teamView: TeamViewNew, catalog: meaningfulCatalog()}
	request, err := json.Marshal(Request{Operation: "team.publish", Draft: &draft})
	if err != nil {
		t.Fatal(err)
	}
	prepared := host.ExchangeSized(request, 0)
	transferred := host.ExchangeSized(request, len(prepared))
	if string(prepared) != string(transferred) {
		t.Fatal("probe and transfer returned different bytes")
	}
	var response Response
	if err := json.Unmarshal(transferred, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Published == nil || response.Published.Revision != 1 {
		t.Fatalf("publish response: %#v", response)
	}
	profiles, err := app.Store.ListProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Revision != 1 {
		t.Fatalf("sizing handshake published more than once: %#v", profiles)
	}
}

func TestGatewayChangePreservesRolesAndClearsEveryModelSelection(t *testing.T) {
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
	draft := meaningfulDraft()
	draft.Gateway = "old"
	host := &Host{ctx: ctx, app: app, launch: Launch{Mode: ModeTeam}, teamView: TeamViewNew, draft: &draft}
	response := host.exchange(Request{Operation: "team.gateway", Gateway: "alpha", Draft: &draft})
	if !response.OK || response.Initial == nil || response.Initial.Draft == nil {
		t.Fatalf("select gateway: %#v", response)
	}
	selected := response.Initial.Draft
	if selected.Name != draft.Name || selected.Primary.ID != "deepseek-coder" || len(selected.Peers) != 1 || len(selected.Specialists) != 1 {
		t.Fatalf("gateway selection lost roles: %#v", selected)
	}
	if selected.Primary.Model.ID != "" || selected.Peers[0].Model.ID != "" || selected.Specialists[0].Model.ID != "" {
		t.Fatalf("old gateway model survived: %#v", selected)
	}
	if len(response.Initial.Catalog) != 3 || response.Initial.Catalog[0].Gateway != "alpha" {
		t.Fatalf("catalog not scoped to alpha: %#v", response.Initial.Catalog)
	}
}
