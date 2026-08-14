package application_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"altv1/internal/application"
	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"

	"github.com/cloudwego/eino/schema"
)

const liveRichInputTeam = `
schema: 1
id: live-rich-input
revision: 1
name: Live Rich Input Verification
gateway: opencode
models:
  router-model: {route: zen, name: nemotron-3.5-lightning-free}
  accountable-text-model: {route: zen, name: deepseek-v4-flash-free}
  alternate-model: {route: zen, name: hy3-free}
  visual-model: {route: zen, name: mimo-v2.5-free}
router:
  model: router-model
  definition: Route requests whose outcome depends on interpreting supplied visual evidence to accountable-lead; route unrelated requests to alternate-lead.
leads:
  - id: accountable-lead
    model: accountable-text-model
    definition: Own answers that require visual evidence, coordinating a visual analyst when the request explicitly asks for independent analysis.
    calls: [visual-analyst]
    peers: [visual-analyst]
  - id: alternate-lead
    model: alternate-model
    definition: Own requests unrelated to interpreting supplied visual evidence.
members:
  - id: visual-analyst
    model: visual-model
    definition: Inspect supplied images precisely and report only observable visual evidence, separating legible labels from inference.
`

func TestLiveOpenCodeImageDelegationAndPeerTransfer(t *testing.T) {
	if os.Getenv("ALT_LIVE_RICH_INPUT") != "1" {
		t.Skip("live free-model gateway test is opt-in")
	}
	dataDir := strings.TrimSpace(os.Getenv("ALT_LIVE_DATA_DIR"))
	imagePath := strings.TrimSpace(os.Getenv("ALT_LIVE_IMAGE"))
	if dataDir == "" || imagePath == "" {
		t.Fatal("ALT_LIVE_DATA_DIR and ALT_LIVE_IMAGE are required")
	}
	expectedEvidence := strings.ToLower(strings.TrimSpace(os.Getenv("ALT_LIVE_IMAGE_EXPECT")))
	if expectedEvidence == "" {
		t.Fatal("ALT_LIVE_IMAGE_EXPECT is required; set it to a word visible in the supplied image")
	}
	requestedScenario := strings.TrimSpace(os.Getenv("ALT_LIVE_SCENARIO"))
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := profile.Parse([]byte(liveRichInputTeam))
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name      string
		request   string
		eventKind event.Kind
	}{
		{
			name:      "stateless specialist",
			request:   "Delegate this image to visual-analyst as a stateless call. Then give me a concise account of the architecture diagram based on that independent report.",
			eventKind: event.DelegationCreated,
		},
		{
			name:      "stateful peer",
			request:   "Open a stateful peer collaboration with visual-analyst, send it this image, and then give me a concise account of the architecture diagram based on the peer's report.",
			eventKind: event.PeerTurnCreated,
		},
	} {
		if requestedScenario != "" && requestedScenario != scenario.name {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			scenarioDir, err := os.MkdirTemp(dataDir, strings.ReplaceAll(scenario.name, " ", "-")+"-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(scenarioDir)
			credentialDir := filepath.Join(dataDir, "credentials")
			if _, statErr := os.Stat(credentialDir); statErr == nil {
				if err := os.Symlink(credentialDir, filepath.Join(scenarioDir, "credentials")); err != nil {
					t.Fatal(err)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal(statErr)
			}
			t.Logf("isolated live data: %s", filepath.Clean(scenarioDir))
			app, err := application.OpenAt(ctx, scenarioDir)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			artifact, err := content.NewImage(imageBytes, imagePath)
			if err != nil {
				t.Fatal(err)
			}
			payload := content.Payload{Input: content.Input{Parts: []content.Part{
				{Type: content.PartText, Text: scenario.request + " "},
				{Type: content.PartAttachment, Attachment: &artifact.ArtifactRef},
			}}, Artifacts: []content.Artifact{artifact}}
			run, err := app.Engine.StartInput(ctx, document, payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			items, err := app.Store.Events(ctx, run.SessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			transferred := false
			answer := ""
			for _, item := range items {
				if item.Kind == scenario.eventKind {
					if item.Kind == event.DelegationCreated {
						spec, decodeErr := event.Decode[event.DelegationSpec](item)
						transferred = decodeErr == nil && len(spec.Attachments) == 1 && spec.Attachments[0] == artifact.Reference
					} else {
						spec, decodeErr := event.Decode[event.PeerTurnSpec](item)
						transferred = decodeErr == nil && len(spec.Attachments) == 1 && spec.Attachments[0] == artifact.Reference
					}
				}
				if item.Kind == event.FinalCompleted {
					final, _ := event.Decode[event.FinalCompletedData](item)
					answer = final.Answer
				}
			}
			if !transferred {
				t.Fatal("Lead did not transfer the immutable image reference through the requested authorized edge")
			}
			if strings.TrimSpace(answer) == "" || !strings.Contains(strings.ToLower(answer), expectedEvidence) {
				t.Fatalf("final answer lacks expected visual evidence %q: %q", expectedEvidence, answer)
			}
		})
	}
}

func TestLiveOpenCodeFreeModelAvailability(t *testing.T) {
	if os.Getenv("ALT_LIVE_RICH_INPUT") != "1" {
		t.Skip("live free-model gateway test is opt-in")
	}
	dataDir := strings.TrimSpace(os.Getenv("ALT_LIVE_DATA_DIR"))
	if dataDir == "" {
		t.Fatal("ALT_LIVE_DATA_DIR is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	app, err := application.OpenAt(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	models := map[string]profile.Model{
		"deepseek":           {Route: "zen", Name: "deepseek-v4-flash-free"},
		"laguna":             {Route: "zen", Name: "laguna-s-2.1-free"},
		"nemotron-ultra":     {Route: "zen", Name: "nemotron-3-ultra-free"},
		"nemotron-lightning": {Route: "zen", Name: "nemotron-3.5-lightning-free"},
		"hy3":                {Route: "zen", Name: "hy3-free"},
		"mimo-vision":        {Route: "zen", Name: "mimo-v2.5-free"},
	}
	p := profile.Profile{Gateway: "opencode", Models: models}
	for reference := range models {
		t.Run(reference, func(t *testing.T) {
			chat, _, err := app.Providers.Model(ctx, p, reference, provider.Text)
			if err != nil {
				t.Fatal(err)
			}
			response, err := chat.Generate(ctx, []*schema.Message{schema.UserMessage("Reply only with OK.")})
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(response.Content) == "" {
				t.Fatal("model returned no text")
			}
		})
	}
}

func TestLiveOpenCodeMiMoReceivesActualImageContent(t *testing.T) {
	if os.Getenv("ALT_LIVE_RICH_INPUT") != "1" {
		t.Skip("live free-model gateway test is opt-in")
	}
	dataDir := strings.TrimSpace(os.Getenv("ALT_LIVE_DATA_DIR"))
	imagePath := strings.TrimSpace(os.Getenv("ALT_LIVE_IMAGE"))
	expectedEvidence := strings.ToLower(strings.TrimSpace(os.Getenv("ALT_LIVE_IMAGE_EXPECT")))
	if dataDir == "" || imagePath == "" || expectedEvidence == "" {
		t.Fatal("ALT_LIVE_DATA_DIR, ALT_LIVE_IMAGE, and ALT_LIVE_IMAGE_EXPECT are required")
	}
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := content.NewImage(imageBytes, imagePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	app, err := application.OpenAt(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	p := profile.Profile{Gateway: "opencode", Models: map[string]profile.Model{
		"vision": {Route: "zen", Name: "mimo-v2.5-free"},
	}}
	chat, _, err := app.Providers.Model(ctx, p, "vision", provider.Text)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(artifact.Data)
	response, err := chat.Generate(ctx, []*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "Name one exact word visibly printed in this image. Reply with only that word."},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &encoded, MIMEType: artifact.MIMEType,
			}}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(response.Content), expectedEvidence) {
		t.Fatalf("MiMo response lacks expected visual evidence %q: %q", expectedEvidence, response.Content)
	}
}
