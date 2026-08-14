package orchestrator

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"altv1/internal/content"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"

	"github.com/cloudwego/eino/schema"
)

func runtimeImage(t *testing.T) content.Artifact {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 5, 4))
	value.Set(1, 2, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	artifact, err := content.NewImage(encoded.Bytes(), "input.png")
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestUnknownVisionCapabilityPreservesImageAndToolFallback(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	document, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	artifact := runtimeImage(t)
	payload := content.Payload{Input: content.Input{Parts: []content.Part{
		{Type: content.PartText, Text: "Explain this: "},
		{Type: content.PartAttachment, Attachment: &artifact.ArtifactRef},
	}}, Artifacts: []content.Artifact{artifact}}
	session, err := ledger.CreateSessionInput(ctx, document, payload, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	runtime := newSessionRuntime(ledger, registry, session, document, nil)
	runtime.engineOptions.ArtifactRoot = t.TempDir()
	message, err := runtime.richUserMessage(ctx, "lead-model", "CURRENT STATE", []string{artifact.Reference})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.UserInputMultiContent) != 2 || message.UserInputMultiContent[1].Type != schema.ChatMessagePartTypeImageURL {
		t.Fatalf("multimodal parts = %#v", message.UserInputMultiContent)
	}
	text := message.UserInputMultiContent[0].Text
	marker := "path "
	position := strings.Index(text, marker)
	if position < 0 {
		t.Fatalf("attachment manifest lacks tool path: %s", text)
	}
	path := strings.Fields(text[position+len(marker):])[0]
	materialized, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(materialized, artifact.Data) {
		t.Fatalf("materialized evidence = (%d bytes, %v)", len(materialized), err)
	}
}
