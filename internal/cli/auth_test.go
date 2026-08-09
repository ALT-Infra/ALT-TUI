package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAuthStatusWithoutNameSummarizesEveryConnection(t *testing.T) {
	var out bytes.Buffer
	err := Execute(
		context.Background(),
		[]string{"--data-dir", t.TempDir(), "auth", "status"},
		strings.NewReader(""), &out, &out,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exa", "linkup", "cline", "fireworks", "opencode", "together", "zenmux"} {
		if !strings.Contains(out.String(), id+": not configured") {
			t.Fatalf("status omitted %s:\n%s", id, out.String())
		}
	}
}

func TestGatewayDescriptorRequiresChoiceWithMultipleGateways(t *testing.T) {
	state := &commandState{dataDir: t.TempDir()}
	_, err := state.gatewayDescriptor(nil)
	if err == nil {
		t.Fatal("gatewayDescriptor(nil) unexpectedly succeeded")
	}
	if !strings.Contains(
		err.Error(),
		"registered gateways: cline, fireworks, opencode, together, zenmux",
	) {
		t.Fatalf("error = %q", err)
	}
}

func TestGatewayDescriptorAcceptsCaseInsensitiveID(t *testing.T) {
	state := &commandState{dataDir: t.TempDir()}
	got, err := state.gatewayDescriptor([]string{"OpenCode"})
	if err != nil {
		t.Fatalf("gatewayDescriptor(opencode): %v", err)
	}
	if got.ID != "opencode" {
		t.Fatalf("descriptor ID = %q, want opencode", got.ID)
	}
}

func TestGatewayDescriptorNeverEchoesUnknownPositionalArgument(t *testing.T) {
	state := &commandState{dataDir: t.TempDir()}
	const secret = "any-credential-shape-may-appear-here"
	_, err := state.gatewayDescriptor([]string{secret})
	if err == nil {
		t.Fatal("gatewayDescriptor(secret) unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error exposed the supplied secret")
	}
	if !strings.Contains(
		err.Error(),
		"registered gateways: cline, fireworks, opencode, together, zenmux",
	) {
		t.Fatalf("error = %q, want registry-derived guidance", err)
	}
}

func TestGatewayDescriptorReportsRegistryForUnknownID(t *testing.T) {
	state := &commandState{dataDir: t.TempDir()}
	_, err := state.gatewayDescriptor([]string{"another-gateway"})
	if err == nil {
		t.Fatal("gatewayDescriptor(unknown) unexpectedly succeeded")
	}
	if !strings.Contains(
		err.Error(),
		"registered gateways: cline, fireworks, opencode, together, zenmux",
	) {
		t.Fatalf("error = %q, want registry-derived guidance", err)
	}
}
