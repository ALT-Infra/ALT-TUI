package provider

import (
	"context"
	"errors"
	"testing"

	"altv1/internal/profile"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type rejectingImageModel struct{ calls int }

func (m *rejectingImageModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	if hasImageInput(input) {
		return nil, errors.New("400: Model only supports text input; received unsupported content type 'image_url'")
	}
	return schema.AssistantMessage("accepted manifest", nil), nil
}

func (m *rejectingImageModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(context.Background(), input)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func (m *rejectingImageModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestUnknownImageSupportRetriesOnlyRecognizedProviderRejection(t *testing.T) {
	base := &rejectingImageModel{}
	registry := NewRegistry()
	spec := profile.Model{Route: "route", Name: "text-only"}
	wrapped := &capabilityAwareModel{base: base, registry: registry, gateway: "gateway", spec: spec}
	encoded := "aW1hZ2U="
	message := &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: "manifest path /evidence.png"},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/png"}}},
	}}
	system := schema.SystemMessage("preserve this exact system contract")
	result, err := wrapped.Generate(context.Background(), []*schema.Message{system, message})
	if err != nil || result.Content != "accepted manifest" || base.calls != 2 {
		t.Fatalf("fallback result = (%#v, %v), calls %d", result, err, base.calls)
	}
	if registry.Capabilities("gateway", spec).ImageInput != CapabilityUnsupported {
		t.Fatal("explicit runtime rejection did not become capability evidence")
	}
	if len(message.UserInputMultiContent) != 2 {
		t.Fatal("fallback mutated the durable caller message")
	}
	stripped := stripImageInput([]*schema.Message{system, message})
	if stripped[0].Content != system.Content || stripped[0].Role != schema.System {
		t.Fatal("image fallback altered an ordinary system message")
	}
}

func TestOpenCodeNoVisionEndpointIsRecognizedCapabilityEvidence(t *testing.T) {
	err := errors.New("400 Bad Request: Upstream request failed: [404] No endpoints found that support image input")
	if !isImageUnsupportedError(err) {
		t.Fatal("OpenCode's explicit no-vision-endpoint response was not recognized")
	}
	if isImageUnsupportedError(errors.New("400 Bad Request: malformed tool schema")) {
		t.Fatal("unrelated provider rejection was treated as image capability evidence")
	}
}

func TestGenericOpenCodeEnvelopeIsLearnedOnlyAfterPathOnlyRetrySucceeds(t *testing.T) {
	base := &genericImageRejectionModel{}
	registry := NewRegistry()
	spec := profile.Model{Route: "route", Name: "unknown-model"}
	wrapper := &capabilityAwareModel{base: base, registry: registry, gateway: "gateway", spec: spec}
	encoded := "aW1hZ2U="
	input := []*schema.Message{{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: "manifest remains"},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/png"}}},
	}}}
	if _, err := wrapper.Generate(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if base.calls != 2 || registry.Capabilities("gateway", spec).ImageInput != CapabilityUnsupported {
		t.Fatal("successful controlled path-only retry did not become capability evidence")
	}
}

type genericImageRejectionModel struct{ calls int }

func (m *genericImageRejectionModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	if hasImageInput(input) {
		return nil, errors.New("Upstream request failed: [400] Provider returned error")
	}
	return schema.AssistantMessage("accepted manifest", nil), nil
}

func (m *genericImageRejectionModel) Stream(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(context.Background(), input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestUnknownCatalogRefreshDoesNotEraseExplicitImageRejection(t *testing.T) {
	registry := NewRegistry()
	factory := &staticGateway{models: []CatalogModel{{
		Gateway: "gateway", Route: "route", ID: "text-only",
		Capabilities: Capabilities{ImageInput: CapabilityUnknown},
	}}}
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	spec := profile.Model{Route: "route", Name: "text-only"}
	registry.markImageUnsupported("gateway", spec)
	if _, err := registry.Catalog(context.Background(), "gateway"); err != nil {
		t.Fatal(err)
	}
	if registry.Capabilities("gateway", spec).ImageInput != CapabilityUnsupported {
		t.Fatal("unknown catalog refresh erased stronger runtime capability evidence")
	}
}

type staticGateway struct{ models []CatalogModel }

func (*staticGateway) Descriptor() GatewayDescriptor {
	return GatewayDescriptor{
		ID: "gateway", Name: "Gateway", CredentialEnvironment: "GATEWAY_KEY",
		Routes: []GatewayRoute{{ID: "route", Label: "Route"}}, MultiModelCatalog: true,
	}
}

func (g *staticGateway) ListModels(context.Context) ([]CatalogModel, error) { return g.models, nil }

func (*staticGateway) NewChatModel(context.Context, profile.Model, Mode) (model.BaseChatModel, error) {
	return &rejectingImageModel{}, nil
}
