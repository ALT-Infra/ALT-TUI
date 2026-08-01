package application

import (
	"reflect"
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
	want := []string{"fireworks", "opencode", "together", "zenmux"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("registered gateways = %#v, want %#v", ids, want)
	}
}
