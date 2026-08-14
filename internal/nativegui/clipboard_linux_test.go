//go:build nativegui && linux && cgo

package nativegui

import (
	"bytes"
	"os"
	"testing"
)

// This test is opt-in because it consumes the user's live desktop clipboard.
// Release verification primes the clipboard with a known image and enables it.
func TestClipboardImageReadsLiveWaylandPNG(t *testing.T) {
	if os.Getenv("ALT_TEST_CLIPBOARD_IMAGE") != "1" {
		t.Skip("live desktop clipboard test is opt-in")
	}
	value, ok := ClipboardImage()
	if !ok {
		t.Fatal("native clipboard bridge found no image")
	}
	if !bytes.HasPrefix(value, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("clipboard returned %d bytes without a PNG signature", len(value))
	}
}
