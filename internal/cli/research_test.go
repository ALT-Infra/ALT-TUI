package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestResearchStatusShowsEveryConnection(t *testing.T) {
	var out bytes.Buffer
	err := Execute(
		context.Background(),
		[]string{"--data-dir", t.TempDir(), "research", "status"},
		strings.NewReader(""), &out, &out,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exa: not configured", "linkup: not configured"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status omitted %q:\n%s", want, out.String())
		}
	}
}

func TestResearchSetRequiresConfiguredConnection(t *testing.T) {
	var out bytes.Buffer
	err := Execute(
		context.Background(),
		[]string{"--data-dir", t.TempDir(), "research", "set", "linkup"},
		strings.NewReader(""), &out, &out,
	)
	if err == nil || !strings.Contains(err.Error(), "alt auth set linkup") {
		t.Fatalf("error = %v, want setup guidance", err)
	}
}
