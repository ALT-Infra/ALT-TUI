package tooling

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
)

func TestLinkupDepthDefaultsToStandard(t *testing.T) {
	depth, err := validateLinkupDepth("")
	if err != nil || depth != "standard" {
		t.Fatalf("default depth = (%q, %v)", depth, err)
	}
	if _, err := validateLinkupDepth("deep-reasoning"); err == nil {
		t.Fatal("Exa-specific depth was accepted for Linkup")
	}
}

func TestLinkupDomainLimitIsValidatedBeforeProviderCall(t *testing.T) {
	domains := make([]string, 101)
	for index := range domains {
		domains[index] = "domain.example"
	}
	// Duplicate normalization means one distinct domain is valid.
	if err := validateLinkupFilters(0, domains); err != nil {
		t.Fatalf("normalized domains: %v", err)
	}
	for index := range domains {
		domains[index] = string(rune('a'+index%26)) + string(rune('A'+index/26)) + ".example"
	}
	if err := validateLinkupFilters(0, domains); err == nil {
		t.Fatal("more than 100 distinct include domains were accepted")
	}
}

func TestSelectedResearchProviderOwnsTheDiscoveredWebSchema(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: "exa", want: "through Exa"},
		{provider: "linkup", want: "through Linkup"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
				ResolveResearchProvider: func(context.Context) (string, error) {
					return test.provider, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if description := discoveredToolDescription(t, runtime, ToolNameWebSearch); !strings.Contains(description, test.want) {
				t.Fatalf("web_search description = %q, want %q", description, test.want)
			}
		})
	}
}

func TestUnselectedResearchProviderRemainsDiscoverableWithSetupGuidance(t *testing.T) {
	runtime, err := NewRuntimeWithOptions(context.Background(), t.TempDir(), RuntimeOptions{
		ResolveResearchProvider: func(context.Context) (string, error) {
			return "", fmt.Errorf("choose /research")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	description := discoveredToolDescription(t, runtime, ToolNameWebSearch)
	if !strings.Contains(description, "unavailable") {
		t.Fatalf("web_search setup description = %q", description)
	}
}

func discoveredToolDescription(t *testing.T, runtime *Runtime, name string) string {
	t.Helper()
	handlers, err := runtime.Handlers(context.Background(), "member:test")
	if err != nil {
		t.Fatal(err)
	}
	runContext := &adk.ChatModelAgentContext{}
	for _, handler := range handlers {
		_, runContext, err = handler.BeforeAgent(context.Background(), runContext)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, runtimeTool := range runContext.Tools {
		info, infoErr := runtimeTool.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == name {
			return info.Desc
		}
	}
	t.Fatalf("tool %q was not discoverable", name)
	return ""
}
