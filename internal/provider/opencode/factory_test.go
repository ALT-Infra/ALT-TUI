package opencode

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

type captureTransport struct {
	request map[string]any
}

func (c *captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	source, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(source, &c.request); err != nil {
		return nil, err
	}
	body := `{
		"id":"completion",
		"object":"chat.completion",
		"created":1,
		"model":"test-model",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"ALT_OK"},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func TestCompletionRequestLeavesEndpointOutputLimitUnset(t *testing.T) {
	t.Setenv("ALT_OPENCODE_API_KEY", "test-secret")
	transport := &captureTransport{}
	factory := NewFactory(credential.NewStore(t.TempDir()))
	factory.HTTPClient = &http.Client{Transport: transport}

	chat, err := factory.NewChatModel(context.Background(), profile.Model{
		Gateway: Name,
		Route:   ZenRoute,
		Name:    "opencode/test-model",
	}, provider.Text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("Reply with ALT_OK."),
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if value, exists := transport.request[key]; exists {
			t.Fatalf("request unexpectedly contains %s=%v: %#v", key, value, transport.request)
		}
	}
	if got := transport.request["model"]; got != "opencode/test-model" {
		t.Fatalf("catalog model ID was rewritten: got %v", got)
	}
	if _, exists := transport.request["response_format"]; exists {
		t.Fatalf("unknown structured-output capability was promoted to supported: %#v", transport.request)
	}
}
