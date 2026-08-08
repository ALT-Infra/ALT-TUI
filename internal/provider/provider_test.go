package provider

import (
	"context"
	"strings"
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
