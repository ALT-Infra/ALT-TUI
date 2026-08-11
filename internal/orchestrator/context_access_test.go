package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"altv1/internal/tooling"
)

func TestBoundedContextOpenPagesExactUTF8WithoutLoss(t *testing.T) {
	content := []byte("begin · exact evidence · النهاية")
	whole := sha256.Sum256(content)
	base := tooling.ContextOpenResult{Reference: "alt://context/records/example", Digest: hex.EncodeToString(whole[:])}

	var recovered []byte
	offset := 0
	for {
		page, err := boundedContextOpen(base, content, offset, 9)
		if err != nil {
			t.Fatal(err)
		}
		if page.Encoding != "utf-8" || page.ByteStart != offset || page.ByteCount != len(content) {
			t.Fatalf("unexpected page metadata: %#v", page)
		}
		chunk := []byte(page.Content)
		digest := sha256.Sum256(chunk)
		if page.ChunkDigest != hex.EncodeToString(digest[:]) {
			t.Fatal("page digest does not cover the returned exact bytes")
		}
		recovered = append(recovered, chunk...)
		if !page.HasMore {
			break
		}
		if page.NextByteOffset <= offset {
			t.Fatalf("pagination did not advance: %#v", page)
		}
		offset = page.NextByteOffset
	}
	if string(recovered) != string(content) {
		t.Fatalf("recovered %q, want %q", recovered, content)
	}
}

func TestBoundedContextOpenRejectsUTF8SplitOffset(t *testing.T) {
	if _, err := boundedContextOpen(tooling.ContextOpenResult{}, []byte("a·b"), 2, 2); err == nil {
		t.Fatal("context open accepted an offset inside a UTF-8 code point")
	}
}

func TestBoundedContextOpenEncodesArbitraryBytesLosslessly(t *testing.T) {
	page, err := boundedContextOpen(tooling.ContextOpenResult{}, []byte{0xff, 0x00, 0x80}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Encoding != "base64" || page.Content != "/wA=" || page.NextByteOffset != 2 {
		t.Fatalf("binary page = %#v", page)
	}
}
