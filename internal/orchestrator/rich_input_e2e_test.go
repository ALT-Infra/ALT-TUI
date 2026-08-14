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

func TestRichImageTraversesAuthorizedStatelessAndStatefulEdges(t *testing.T) {
	for _, peer := range []bool{false, true} {
		name := "stateless specialist"
		if peer {
			name = "stateful peer"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ledger, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			artifact := runtimeImage(t)
			factory := &richImageScenarioFactory{reference: artifact.Reference, peer: peer}
			registry := provider.NewRegistry()
			if err := registry.Register(factory); err != nil {
				t.Fatal(err)
			}
			document, err := profile.Parse([]byte(testProfile))
			if err != nil {
				t.Fatal(err)
			}
			payload := content.Payload{Input: content.Input{Parts: []content.Part{
				{Type: content.PartText, Text: "Describe this architecture: "},
				{Type: content.PartAttachment, Attachment: &artifact.ArtifactRef},
			}}, Artifacts: []content.Artifact{artifact}}
			run, err := NewEngine(ledger, registry).StartInput(ctx, document, payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Wait(ctx); err != nil {
				logEvents(t, ledger, run.SessionID)
				t.Fatal(err)
			}
			factory.mu.Lock()
			routerSawImage, collaboratorSawImage := factory.routerSawImage, factory.collaboratorSawImage
			factory.mu.Unlock()
			if !routerSawImage || !collaboratorSawImage {
				t.Fatalf("multimodal delivery = router %v collaborator %v", routerSawImage, collaboratorSawImage)
			}
			items, err := ledger.Events(ctx, run.SessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			transferred := false
			for _, item := range items {
				if !peer && item.Kind == event.DelegationCreated {
					spec, decodeErr := event.Decode[event.DelegationSpec](item)
					transferred = decodeErr == nil && len(spec.Attachments) == 1 && spec.Attachments[0] == artifact.Reference
				}
				if peer && item.Kind == event.PeerTurnCreated {
					spec, decodeErr := event.Decode[event.PeerTurnSpec](item)
					transferred = decodeErr == nil && len(spec.Attachments) == 1 && spec.Attachments[0] == artifact.Reference
				}
			}
			if !transferred {
				t.Fatal("durable authorized edge omitted the selected attachment reference")
			}
			session, err := ledger.Session(ctx, run.SessionID)
			if err != nil || session.FinalAnswer != "The diagram visibly labels the router." {
				t.Fatalf("accountable final = %q, %v", session.FinalAnswer, err)
			}
		})
	}
}

type richImageScenarioFactory struct {
	mu                   sync.Mutex
	reference            string
	peer                 bool
	leadCalls            int
	routerSawImage       bool
	collaboratorSawImage bool
}

func (*richImageScenarioFactory) Descriptor() provider.GatewayDescriptor {
	return testGatewayDescriptor()
}

func (*richImageScenarioFactory) ListModels(context.Context) ([]provider.CatalogModel, error) {
	return testCatalog(), nil
}

func (f *richImageScenarioFactory) NewChatModel(_ context.Context, spec profile.Model, _ provider.Mode) (model.BaseChatModel, error) {
	return &richImageScenarioModel{factory: f, name: spec.Name}, nil
}

type richImageScenarioModel struct {
	factory *richImageScenarioFactory
	name    string
}

func (m *richImageScenarioModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *richImageScenarioModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if m.name != "router" {
		return nil, fmt.Errorf("unexpected Generate call for %s", m.name)
	}
	m.factory.mu.Lock()
	m.factory.routerSawImage = messagesContainImage(input)
	m.factory.mu.Unlock()
	return response(`{"lead_id":"architecture-lead","confidence":1,"basis":"The accountable architecture Lead owns the supplied visual evidence."}`), nil
}

func (m *richImageScenarioModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	switch m.name {
	case "research":
		m.factory.mu.Lock()
		m.factory.collaboratorSawImage = messagesContainImage(input)
		m.factory.mu.Unlock()
		return schema.StreamReaderFromArray([]*schema.Message{response(`{"result":"The image visibly labels the router.","findings":["router label"],"risks":[],"confidence":1}`)}), nil
	case "lead":
		m.factory.mu.Lock()
		m.factory.leadCalls++
		call := m.factory.leadCalls
		reference, peer := m.factory.reference, m.factory.peer
		m.factory.mu.Unlock()
		if call == 1 {
			decision := fmt.Sprintf(`{"assessment":"Independent visual evidence is needed.","delegations":[{"key":"visual","member_id":"research","objective":"Inspect the supplied image and report visible labels.","context":"","attachments":[%q],"depends_on":[]}],"peer_turns":[],"cancel":[],"finalize":false,"final_brief":""}`, reference)
			if peer {
				decision = fmt.Sprintf(`{"assessment":"A stateful visual collaboration is useful.","delegations":[],"peer_turns":[{"key":"visual","peer_id":"research","collaboration_id":"","objective":"Inspect the supplied image and report visible labels.","context":"","attachments":[%q]}],"cancel":[],"finalize":false,"final_brief":""}`, reference)
			}
			return schema.StreamReaderFromArray([]*schema.Message{response(decision)}), nil
		}
		if call == 2 {
			return schema.StreamReaderFromArray([]*schema.Message{response(`{"assessment":"The visual report is sufficient.","delegations":[],"peer_turns":[],"cancel":[],"finalize":true,"final_brief":"Report the visible router label."}`)}), nil
		}
		return schema.StreamReaderFromArray([]*schema.Message{response("The diagram visibly labels the router.")}), nil
	default:
		return nil, fmt.Errorf("unexpected Stream call for %s", m.name)
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
