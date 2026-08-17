package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestBlindCodingPrimaryUsesExplicitlySelectedVisionSpecialist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	artifact := runtimeImage(t)
	factory := &blindCoderVisionFactory{reference: artifact.Reference}
	registry := provider.NewRegistry()
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	document, err := profile.FromValue(visionCodingProfile(false))
	if err != nil {
		t.Fatal(err)
	}
	request := "Fix the code using the exact compiler diagnostic in this screenshot: "
	payload := content.Payload{Input: content.Input{Parts: []content.Part{
		{Type: content.PartText, Text: request},
		{Type: content.PartAttachment, Attachment: &artifact.ArtifactRef},
	}}, Artifacts: []content.Artifact{artifact}}
	run, err := NewEngine(ledger, registry).StartInputAt(ctx, document, payload, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(ctx); err != nil {
		logEvents(t, ledger, run.SessionID)
		t.Fatal(err)
	}
	factory.mu.Lock()
	primarySawImage := factory.primarySawImage
	primarySawExactText := factory.primarySawExactText
	specialistSawImage := factory.specialistSawImage
	factory.mu.Unlock()
	if primarySawImage {
		t.Fatal("catalog-declared blind coding primary received image bytes")
	}
	if !primarySawExactText {
		t.Fatal("blind coding primary did not receive the user's exact text")
	}
	if !specialistSawImage {
		t.Fatal("explicitly selected vision specialist did not receive image bytes")
	}
	events, err := ledger.Events(ctx, run.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var spec event.DelegationSpec
	for _, item := range events {
		if item.Kind == event.DelegationCreated {
			spec, err = event.Decode[event.DelegationSpec](item)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if spec.CallerID != "deepseek-coder" || spec.SpecialistID != "vision-specialist" || len(spec.Attachments) != 1 || spec.Attachments[0] != artifact.Reference {
		t.Fatalf("durable specialist call did not preserve explicit authority: %#v", spec)
	}
	session, err := ledger.Session(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.FinalAnswer != "I fixed the undefined symbol reported at parser.go:42." {
		t.Fatalf("final answer = %q", session.FinalAnswer)
	}
	if _, exists, err := ledger.Get(ctx, modelSurfaceKey(session.ConversationID, "vision-specialist")); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("stateless specialist acquired a durable model surface")
	}
}

type blindCoderVisionFactory struct {
	mu                  sync.Mutex
	reference           string
	primaryCalls        int
	primarySawImage     bool
	primarySawExactText bool
	specialistSawImage  bool
}

func (*blindCoderVisionFactory) Descriptor() provider.GatewayDescriptor {
	return testGatewayDescriptor()
}

func (*blindCoderVisionFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return []provider.CatalogModel{
		{Gateway: "opencode", Route: "test", ID: "deepseek-code", Capabilities: provider.Capabilities{ToolCalling: provider.CapabilitySupported, ImageInput: provider.CapabilityUnsupported}},
		{Gateway: "opencode", Route: "test", ID: "research", Capabilities: provider.Capabilities{ToolCalling: provider.CapabilitySupported}},
		{Gateway: "opencode", Route: "test", ID: "vision", Capabilities: provider.Capabilities{ToolCalling: provider.CapabilityUnsupported, ImageInput: provider.CapabilitySupported}},
	}, nil
}

func (f *blindCoderVisionFactory) NewChatModel(_ context.Context, spec profile.Model, _ provider.Mode) (model.BaseChatModel, error) {
	return &blindCoderVisionModel{factory: f, name: spec.Name}, nil
}

type blindCoderVisionModel struct {
	factory *blindCoderVisionFactory
	name    string
}

func (m *blindCoderVisionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *blindCoderVisionModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return response(m.reply(input)), nil
}

func (m *blindCoderVisionModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{response(m.reply(input))}), nil
}

func (m *blindCoderVisionModel) reply(input []*schema.Message) string {
	switch m.name {
	case "deepseek-code":
		m.factory.mu.Lock()
		m.factory.primaryCalls++
		call := m.factory.primaryCalls
		m.factory.primarySawImage = m.factory.primarySawImage || messagesContainImage(input)
		m.factory.primarySawExactText = m.factory.primarySawExactText || exactUserText(input) == "Fix the code using the exact compiler diagnostic in this screenshot: "
		reference := m.factory.reference
		m.factory.mu.Unlock()
		if call == 1 {
			return fmt.Sprintf(`{"kind":"coordinate","assessment":"The coding primary needs the pixels transcribed.","delegations":[{"key":"read-diagnostic","specialist_id":"vision-specialist","objective":"Transcribe the compiler diagnostic, filename, and line number visible in the attached screenshot.","context":"Return observable text only.","attachments":[%q],"depends_on":[]}],"peer_turns":[],"cancel":[],"handoff":null}`, reference)
		}
		return "I fixed the undefined symbol reported at parser.go:42."
	case "vision":
		m.factory.mu.Lock()
		m.factory.specialistSawImage = m.factory.specialistSawImage || messagesContainImage(input)
		m.factory.mu.Unlock()
		return `{"result":"parser.go:42: undefined: tokenKind","findings":["compiler diagnostic transcribed"],"risks":[],"confidence":1}`
	default:
		return "unexpected model"
	}
}

func messagesContainImage(messages []*schema.Message) bool {
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, part := range message.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL && part.Image != nil && part.Image.Base64Data != nil {
				return true
			}
		}
	}
	return false
}
