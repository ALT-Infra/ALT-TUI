package store_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/store"
)

func artifactPNG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 4, 3))
	value.Set(2, 1, color.RGBA{R: 80, G: 120, B: 200, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestArtifactsAreAtomicImmutableAndConversationScoped(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	document := mustProfile(t)
	if err := ledger.ImportProfile(ctx, document); err != nil {
		t.Fatal(err)
	}
	artifact, err := content.NewImage(artifactPNG(t), "clipboard.png")
	if err != nil {
		t.Fatal(err)
	}
	payload := content.Payload{
		Input: content.Input{Parts: []content.Part{
			{Type: content.PartText, Text: "Inspect "},
			{Type: content.PartAttachment, Attachment: &artifact.ArtifactRef},
		}},
		Artifacts: []content.Artifact{artifact},
	}
	first, err := ledger.CreateSessionInput(ctx, document, payload, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ledger.Artifact(ctx, first.ID, artifact.Reference)
	if err != nil || !bytes.Equal(resolved.Data, artifact.Data) {
		t.Fatalf("resolved artifact = (%d bytes, %v)", len(resolved.Data), err)
	}
	if _, err := ledger.Append(ctx, first.ID, event.Draft{
		Kind: event.FinalCompleted, Actor: "lead",
		Data: event.FinalCompletedData{Answer: "done"},
	}); err != nil {
		t.Fatal(err)
	}
	continuation, err := ledger.CreateContinuation(ctx, first.ID, document, "Continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Artifact(ctx, continuation.ID, artifact.Reference); err != nil {
		t.Fatalf("continuation cannot resolve conversation evidence: %v", err)
	}
	other, err := ledger.CreateSession(ctx, document, "Unrelated", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Artifact(ctx, other.ID, artifact.Reference); err == nil {
		t.Fatal("attachment escaped into an unrelated conversation")
	}
	events, err := ledger.Events(ctx, first.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	created, err := event.Decode[event.SessionCreatedData](events[0])
	if err != nil || created.Input.DisplayText() != "Inspect [Image #1]" {
		t.Fatalf("durable input = (%q, %v)", created.Input.DisplayText(), err)
	}
}
