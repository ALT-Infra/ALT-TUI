package application

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestGatewayRegistryContainsEverySupportedMultiModelGateway(t *testing.T) {
	registry, err := NewGatewayRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	descriptors := registry.Descriptors()
	ids := make([]string, 0, len(descriptors))
	credentialVariables := make(map[string]string, len(descriptors))
	for _, descriptor := range descriptors {
		ids = append(ids, descriptor.ID)
		if !descriptor.MultiModelCatalog {
			t.Fatalf("%s is registered without a multi-model catalog", descriptor.ID)
		}
		if previous := credentialVariables[descriptor.CredentialEnvironment]; previous != "" {
			t.Fatalf(
				"%s and %s share credential variable %s",
				previous,
				descriptor.ID,
				descriptor.CredentialEnvironment,
			)
		}
		credentialVariables[descriptor.CredentialEnvironment] = descriptor.ID
	}
	want := []string{"cline", "fireworks", "opencode", "together", "zenmux"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("registered gateways = %#v, want %#v", ids, want)
	}
}

func TestSoleConfiguredResearchProviderIsSelectedWithoutCeremony(t *testing.T) {
	t.Setenv("EXA_API_KEY", "exa-secret")
	t.Setenv("LINKUP_API_KEY", "")
	app, err := OpenAt(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	provider, err := app.ResolveResearchProvider(context.Background())
	if err != nil || provider != "exa" {
		t.Fatalf("provider = (%q, %v)", provider, err)
	}
}

func TestMultipleResearchProvidersRequireAndPersistAChoice(t *testing.T) {
	t.Setenv("EXA_API_KEY", "exa-secret")
	t.Setenv("LINKUP_API_KEY", "linkup-secret")
	dataDir := t.TempDir()
	app, err := OpenAt(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.ResolveResearchProvider(context.Background()); err == nil || !strings.Contains(err.Error(), "/research") {
		t.Fatalf("ambiguous provider error = %v", err)
	}
	if err := app.SelectResearchProvider(context.Background(), "linkup"); err != nil {
		t.Fatal(err)
	}
	app.Close()

	reopened, err := OpenAt(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	provider, err := reopened.ResolveResearchProvider(context.Background())
	if err != nil || provider != "linkup" {
		t.Fatalf("persisted provider = (%q, %v)", provider, err)
	}
}
