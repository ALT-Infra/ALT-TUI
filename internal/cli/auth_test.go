package cli

import (
	"strings"
	"testing"
)

func TestGatewayDescriptorRequiresChoiceWithMultipleGateways(t *testing.T) {
	state := &commandState{dataDir: t.TempDir()}
	_, err := state.gatewayDescriptor(nil)
	if err == nil {
		t.Fatal("gatewayDescriptor(nil) unexpectedly succeeded")
	}
	if !strings.Contains(
		err.Error(),
		"registered gateways: fireworks, opencode, together, zenmux",
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
		"registered gateways: fireworks, opencode, together, zenmux",
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
		"registered gateways: fireworks, opencode, together, zenmux",
	) {
		t.Fatalf("error = %q, want registry-derived guidance", err)
	}
}
