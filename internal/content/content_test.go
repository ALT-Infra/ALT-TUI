package content

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(1, 1, color.RGBA{R: 12, G: 34, B: 56, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestNewImageProducesPortableImmutableMetadata(t *testing.T) {
	data := testPNG(t)
	artifact, err := NewImage(data, "/tmp/example.png")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Kind != ArtifactImage || artifact.MIMEType != "image/png" || artifact.Width != 3 || artifact.Height != 2 {
		t.Fatalf("artifact = %#v", artifact.ArtifactRef)
	}
	if artifact.Name != "example.png" || artifact.ByteCount != len(data) || len(artifact.Digest) != 64 {
		t.Fatalf("artifact metadata = %#v", artifact.ArtifactRef)
	}
	data[0] = 0
	if artifact.Data[0] == 0 {
		t.Fatal("artifact retained caller-owned bytes")
	}
}

func TestDisplayTextPreservesPartOrderAndNumbersAttachments(t *testing.T) {
	first := ArtifactRef{Reference: "artifact:1", Kind: ArtifactImage}
	second := ArtifactRef{Reference: "artifact:2", Kind: ArtifactImage}
	input := Input{Parts: []Part{
		{Type: PartText, Text: "Compare "},
		{Type: PartAttachment, Attachment: &first},
		{Type: PartText, Text: " with "},
		{Type: PartAttachment, Attachment: &second},
	}}
	if got := input.DisplayText(); got != "Compare [Image #1] with [Image #2]" {
		t.Fatalf("display = %q", got)
	}
}

func TestPayloadRejectsMissingMismatchedAndUnreferencedArtifacts(t *testing.T) {
	artifact, err := NewImage(testPNG(t), "one.png")
	if err != nil {
		t.Fatal(err)
	}
	part := Part{Type: PartAttachment, Attachment: &artifact.ArtifactRef}
	valid := Payload{Input: Input{Parts: []Part{part}}, Artifacts: []Artifact{artifact}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if err := (Payload{Input: valid.Input}).Validate(); err == nil {
		t.Fatal("referenced attachment without bytes was accepted")
	}
	forged := artifact.ArtifactRef
	forged.Digest = strings.Repeat("0", 64)
	if err := (Payload{Input: Input{Parts: []Part{{Type: PartAttachment, Attachment: &forged}}}, Artifacts: []Artifact{artifact}}).Validate(); err == nil {
		t.Fatal("mismatched durable metadata was accepted")
	}
	if err := (Payload{Input: Text("plain"), Artifacts: []Artifact{artifact}}).Validate(); err == nil {
		t.Fatal("unreferenced binary evidence was accepted")
	}
}
