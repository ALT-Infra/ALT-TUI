package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"altv1/internal/credential"
	"altv1/internal/profile"
	"altv1/internal/provider"

	"github.com/cloudwego/eino/schema"
)

type completionTransport struct {
	request  *http.Request
	body     map[string]any
	response string
}

func (c *completionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.request = request
	source, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(source, &c.body); err != nil {
		return nil, err
	}
	response := c.response
	if response == "" {
		response = `{
		"id":"completion",
		"object":"chat.completion",
		"created":1,
		"model":"provider/model",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"ALT_OK"},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`
	}
	return jsonResponse(request, response), nil
}

func TestExecutionPreservesCatalogIdentityAndEndpointLimits(t *testing.T) {
	t.Setenv("ALT_TEST_GATEWAY_KEY", "secret")
	transport := &completionTransport{}
	factory := NewFactory(credential.NewStore(t.TempDir()), Config{
		Descriptor: provider.GatewayDescriptor{
			ID:                    "test-gateway",
			Name:                  "Test Gateway",
			CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
			MultiModelCatalog:     true,
			Routes: []provider.GatewayRoute{
				{ID: "serverless", Label: "Serverless"},
			},
		},
		Route:    "serverless",
		BaseURL:  "https://gateway.example/v1",
		Hostname: "gateway.example",
	})
	factory.HTTPClient = &http.Client{Transport: transport}
	chat, err := factory.NewChatModel(context.Background(), profile.Model{
		Route: "serverless",
		Name:  "provider/model",
	}, provider.Text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("Reply with ALT_OK."),
	}); err != nil {
		t.Fatal(err)
	}
	if got := transport.request.URL.String(); got != "https://gateway.example/v1/chat/completions" {
		t.Fatalf("completion URL = %q", got)
	}
	if got := transport.request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := transport.body["model"]; got != "provider/model" {
		t.Fatalf("catalog model ID rewritten to %v", got)
	}
	for _, key := range []string{
		"max_completion_tokens",
		"max_tokens",
		"response_format",
	} {
		if value, exists := transport.body[key]; exists {
			t.Fatalf("request unexpectedly contains %s=%v", key, value)
		}
	}
}

func TestExecutionRejectsUnknownRouteBeforeResolvingCredential(t *testing.T) {
	factory := NewFactory(credential.NewStore(t.TempDir()), Config{
		Descriptor: provider.GatewayDescriptor{
			ID:                    "test-gateway",
			Name:                  "Test Gateway",
			CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
			MultiModelCatalog:     true,
			Routes: []provider.GatewayRoute{
				{ID: "serverless", Label: "Serverless"},
			},
		},
		Route:    "serverless",
		BaseURL:  "https://gateway.example/v1",
		Hostname: "gateway.example",
	})
	_, err := factory.NewChatModel(context.Background(), profile.Model{
		Route: "invented",
		Name:  "provider/model",
	}, provider.Text)
	if err == nil || !strings.Contains(err.Error(), `unknown Test Gateway catalog route "invented"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestDocumentedCacheAffinityUsesStableOpaqueScope(t *testing.T) {
	t.Setenv("ALT_TEST_GATEWAY_KEY", "secret")
	transport := &completionTransport{}
	factory := NewFactory(credential.NewStore(t.TempDir()), Config{
		Descriptor: provider.GatewayDescriptor{
			ID: "affinity-gateway", Name: "Affinity Gateway",
			CredentialEnvironment: "ALT_TEST_GATEWAY_KEY", MultiModelCatalog: true,
			Routes: []provider.GatewayRoute{{ID: "serverless", Label: "Serverless"}},
		},
		Route: "serverless", BaseURL: "https://gateway.example/v1", Hostname: "gateway.example",
		PromptCacheAffinity: true,
	})
	factory.HTTPClient = &http.Client{Transport: transport}
	ctx := provider.WithCacheScope(context.Background(), "conversation-1:agent:coder")
	chat, err := factory.NewChatModel(ctx, profile.Model{Route: "serverless", Name: "provider/model"}, provider.Text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}); err != nil {
		t.Fatal(err)
	}
	want := provider.CacheAffinityKey(ctx)
	if got := transport.body["prompt_cache_key"]; got != want || want == "" {
		t.Fatalf("prompt_cache_key = %v, want %q", got, want)
	}
	if strings.Contains(want, "conversation-1") || strings.Contains(want, "coder") {
		t.Fatalf("cache affinity key exposed scope: %q", want)
	}
}

func TestGatewayCacheTelemetrySurvivesTheGenericOpenAITranslator(t *testing.T) {
	t.Setenv("ALT_TEST_GATEWAY_KEY", "secret")
	transport := &completionTransport{response: `{
		"id":"completion","object":"chat.completion","created":1,"model":"provider/model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":90,"completion_tokens":1,"total_tokens":91,"cached_tokens":64}
	}`}
	factory := NewFactory(credential.NewStore(t.TempDir()), Config{
		Descriptor: provider.GatewayDescriptor{
			ID: "cache-gateway", Name: "Cache Gateway", CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
			MultiModelCatalog: true, Routes: []provider.GatewayRoute{{ID: "serverless", Label: "Serverless"}},
		},
		Route: "serverless", BaseURL: "https://gateway.example/v1", Hostname: "gateway.example",
	})
	factory.HTTPClient = &http.Client{Transport: transport}
	chat, err := factory.NewChatModel(context.Background(), profile.Model{Route: "serverless", Name: "provider/model"}, provider.Text)
	if err != nil {
		t.Fatal(err)
	}
	message, err := chat.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.CacheUsageFromMessage(message); got != (provider.CacheUsage{Reported: true, ReadTokens: 64}) {
		t.Fatalf("translated cache usage = %#v", got)
	}
}

func TestDocumentedExplicitCacheControlAppliesOnlyToConfiguredModelPrefixes(t *testing.T) {
	t.Setenv("ALT_TEST_GATEWAY_KEY", "secret")
	for _, test := range []struct {
		name, model string
		wantControl bool
	}{
		{name: "documented family", model: "anthropic/claude-example", wantControl: true},
		{name: "other family", model: "openai/example", wantControl: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &completionTransport{}
			factory := NewFactory(credential.NewStore(t.TempDir()), Config{
				Descriptor: provider.GatewayDescriptor{
					ID: "explicit-gateway", Name: "Explicit Gateway", CredentialEnvironment: "ALT_TEST_GATEWAY_KEY",
					MultiModelCatalog: true, Routes: []provider.GatewayRoute{{ID: "serverless", Label: "Serverless"}},
				},
				Route: "serverless", BaseURL: "https://gateway.example/v1", Hostname: "gateway.example",
				ExplicitCacheControlPrefixes: []string{"anthropic/"},
			})
			factory.HTTPClient = &http.Client{Transport: transport}
			chat, err := factory.NewChatModel(context.Background(), profile.Model{Route: "serverless", Name: test.model}, provider.Text)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := chat.Generate(context.Background(), []*schema.Message{
				schema.SystemMessage("stable"), schema.UserMessage("variable"),
			}); err != nil {
				t.Fatal(err)
			}
			messages := transport.body["messages"].([]any)
			system := messages[0].(map[string]any)
			_, hasBlocks := system["content"].([]any)
			if hasBlocks != test.wantControl {
				t.Fatalf("explicit cache-control blocks present = %t, want %t; body = %#v", hasBlocks, test.wantControl, transport.body)
			}
		})
	}
}

func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
