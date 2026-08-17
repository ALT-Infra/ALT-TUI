package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"altv1/internal/profile"

	"github.com/cloudwego/eino/components/model"
)

type directLabAdapter struct{}

func (directLabAdapter) Descriptor() GatewayDescriptor {
	return GatewayDescriptor{
		ID: "single-lab", Name: "Single Lab",
		CredentialEnvironment: "SINGLE_LAB_KEY",
		Routes:                []GatewayRoute{{ID: "default", Label: "Default"}},
		MultiModelCatalog:     false,
	}
}

type metadataCatalogGateway struct{}

func (metadataCatalogGateway) Descriptor() GatewayDescriptor {
	return GatewayDescriptor{
		ID: "metadata-gateway", Name: "Metadata Gateway",
		CredentialEnvironment: "METADATA_GATEWAY_KEY",
		Routes: []GatewayRoute{{
			ID: "serverless", Label: "Serverless", MetadataCatalog: "catalog-route",
		}},
		MultiModelCatalog: true,
	}
}

func (metadataCatalogGateway) ListModels(context.Context) ([]CatalogModel, error) {
	return []CatalogModel{{
		Gateway: "metadata-gateway", Route: "serverless", ID: "exact/model",
		Capabilities: Capabilities{ToolCalling: CapabilityUnknown},
		Limits: ModelLimits{
			ContextWindow: NewTokenLimit(64_000, LimitSourceGatewayCatalog),
		},
	}}, nil
}

func (metadataCatalogGateway) NewChatModel(context.Context, profile.Model, Mode) (model.BaseChatModel, error) {
	return nil, nil
}

func TestLiveMetadataRevalidatesWithoutOwningModelIdentity(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("If-None-Match") == `"catalog-v1"` {
			response.WriteHeader(http.StatusNotModified)
			return
		}
		response.Header().Set("ETag", `"catalog-v1"`)
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{
			"catalog-route":{"models":{
				"exact/model":{"id":"exact/model","name":"Exact Model","tool_call":true,
					"limit":{"context":128000,"input":96000,"output":8192},
					"modalities":{"input":["text","image"],"output":["text"]}},
				"metadata-only/model":{"id":"metadata-only/model","name":"Must Not Appear","limit":{"context":1000000}}
			}}
		}`)
	}))
	defer server.Close()

	metadata := NewModelsDevSource(server.Client())
	metadata.url = server.URL
	registry := NewRegistryWithOptions(RegistryOptions{Metadata: metadata})
	if err := registry.Register(metadataCatalogGateway{}); err != nil {
		t.Fatal(err)
	}

	for pass := 0; pass < 2; pass++ {
		catalog, err := registry.Catalog(context.Background(), "metadata-gateway")
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog) != 1 || catalog[0].ID != "exact/model" {
			t.Fatalf("metadata changed authenticated identities: %#v", catalog)
		}
		if catalog[0].DisplayName != "Exact Model" ||
			catalog[0].Capabilities.ToolCalling != CapabilitySupported ||
			catalog[0].Capabilities.ImageInput != CapabilitySupported {
			t.Fatalf("live metadata was not applied: %#v", catalog[0])
		}
		limits := catalog[0].Limits
		if limits.ContextWindow.Tokens != 64_000 || limits.ContextWindow.Source != LimitSourceGatewayCatalog {
			t.Fatalf("stricter authenticated context limit lost: %#v", limits.ContextWindow)
		}
		if limits.MaxInput.Tokens != 96_000 || limits.MaxOutput.Tokens != 8_192 {
			t.Fatalf("published input/output limits missing: %#v", limits)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("metadata requests = %d, want initial fetch plus validator revalidation", requests.Load())
	}
}
func (directLabAdapter) ListModels(context.Context) ([]CatalogModel, error) {
	return nil, nil
}
func (directLabAdapter) NewChatModel(
	context.Context,
	profile.Model,
	Mode,
) (model.BaseChatModel, error) {
	return nil, nil
}

func TestRegistryRejectsDirectLabCredentialAdapters(t *testing.T) {
	err := NewRegistry().Register(directLabAdapter{})
	if err == nil || !strings.Contains(err.Error(), "multi-model catalog") {
		t.Fatalf("direct lab adapter crossed the gateway boundary: %v", err)
	}
}

type catalogGateway struct{}

func (catalogGateway) Descriptor() GatewayDescriptor {
	return GatewayDescriptor{
		ID: "gateway", Name: "Gateway",
		CredentialEnvironment: "GATEWAY_KEY",
		Routes:                []GatewayRoute{{ID: "serverless", Label: "Serverless"}},
		MultiModelCatalog:     true,
	}
}
func (catalogGateway) ListModels(context.Context) ([]CatalogModel, error) {
	return []CatalogModel{{
		Gateway: "gateway", Route: "serverless", ID: "exact/model",
		Capabilities: Capabilities{
			StructuredOutput: CapabilitySupported,
			ToolCalling:      CapabilityUnsupported,
		},
	}}, nil
}
func (catalogGateway) NewChatModel(context.Context, profile.Model, Mode) (model.BaseChatModel, error) {
	return nil, nil
}

func TestAuthenticatedCatalogControlsIdentityAndCapabilities(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(catalogGateway{}); err != nil {
		t.Fatal(err)
	}
	value := profile.Profile{Gateway: "gateway", Models: map[string]profile.Model{
		"member": {Route: "serverless", Name: "exact/model"},
	}}
	if err := registry.ValidateProfile(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	capabilities := registry.Capabilities(value.Gateway, value.Models["member"])
	if capabilities.StructuredOutput != CapabilitySupported ||
		capabilities.ToolCalling != CapabilityUnsupported {
		t.Fatalf("catalog capabilities were not preserved: %#v", capabilities)
	}
	changed := value
	changed.Models = map[string]profile.Model{
		"member": {Route: "serverless", Name: "rewritten-model"},
	}
	err := registry.ValidateProfile(context.Background(), changed)
	if err == nil || !strings.Contains(err.Error(), "will not substitute") {
		t.Fatalf("missing exact selection was accepted: %v", err)
	}
}
