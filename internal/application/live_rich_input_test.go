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

const liveSpecialistTeam = `
schema: 2
id: live-visual-specialist
revision: 1
name: Blind coder with stateless visual inspection
gateway: opencode
models:
  code-model: {route: zen, name: deepseek-v4-flash-free}
  visual-model: {route: zen, name: mimo-v2.5-free}
primary:
  id: deepseek-coder
  model: code-model
  definition: Own implementation end to end. When required evidence exists only in attached pixels, call visual-inspector with a self-contained question and the exact attachment, then continue the code task yourself.
  specialists: [visual-inspector]
specialists:
  - id: visual-inspector
    model: visual-model
    definition: Begin from a clean slate, inspect only media explicitly attached to this invocation, and return precise observable evidence to the caller without owning the broader task.
`

const liveVisualPeerTeam = `
schema: 2
id: live-visual-peer
revision: 1
name: Multimodal analysis handoff
gateway: opencode
models:
  entry-model: {route: zen, name: hy3-free}
  visual-model: {route: zen, name: mimo-v2.5-free}
primary:
  id: entry
  model: entry-model
  definition: This is the entry point for every user turn. Hand leadership to visual-analyst when the user's requested end result is interpretation of attached visual material; otherwise own the result.
  peers: [visual-analyst]
peers:
  - id: visual-analyst
    model: visual-model
    definition: Own end-to-end requests whose requested result is visual analysis. Inspect the original attached media, separate legible content and observable structure from inference, and answer the user directly.
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
	for _, scenario := range []struct {
		name       string
		team       string
		request    string
		eventKind  event.Kind
		finalActor string
	}{
		{
			name:       "stateless specialist",
			team:       liveSpecialistTeam,
			request:    "Use visual-inspector as a stateless specialist to read this architecture diagram, then give me a concise implementation-oriented account based on its report.",
			eventKind:  event.DelegationCreated,
			finalActor: "deepseek-coder",
		},
		{
			name:       "peer leadership handoff",
			team:       liveVisualPeerTeam,
			request:    "This request's end result is visual analysis. Hand leadership to visual-analyst, which should inspect this architecture diagram and answer me directly with a concise account.",
			eventKind:  event.LeadershipTransferred,
			finalActor: "visual-analyst",
		},
	} {
		if requestedScenario != "" && requestedScenario != scenario.name {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			document, err := profile.Parse([]byte(scenario.team))
			if err != nil {
				t.Fatal(err)
			}
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
			authorityObserved := false
			answer := ""
			finalActor := ""
			for _, item := range items {
				if item.Kind == scenario.eventKind {
					if item.Kind == event.DelegationCreated {
						spec, decodeErr := event.Decode[event.DelegationSpec](item)
						authorityObserved = decodeErr == nil && spec.CallerID == "deepseek-coder" && spec.SpecialistID == "visual-inspector" && len(spec.Attachments) == 1 && spec.Attachments[0] == artifact.Reference
					} else {
						transfer, decodeErr := event.Decode[event.LeadershipTransferredData](item)
						authorityObserved = decodeErr == nil && transfer.FromAgentID == "entry" && transfer.ToAgentID == "visual-analyst"
					}
				}
				if item.Kind == event.FinalCompleted {
					final, _ := event.Decode[event.FinalCompletedData](item)
					answer = final.Answer
					finalActor = item.Actor
				}
			}
			if !authorityObserved {
				for _, item := range items {
					t.Logf("%03d %-28s actor=%s data=%s", item.Sequence, item.Kind, item.Actor, item.Data)
				}
				t.Fatal("the requested specialist call or leadership transfer was not recorded on its authorized edge")
			}
			if finalActor != scenario.finalActor {
				t.Fatalf("final answer actor = %q, want %q", finalActor, scenario.finalActor)
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
