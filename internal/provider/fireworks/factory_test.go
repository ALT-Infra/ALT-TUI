package fireworks

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"altv1/internal/credential"
	"altv1/internal/provider"
)

type pagedTransport struct {
	requests []*http.Request
}

func (c *pagedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, request)
	body := `{"models":[
		{"name":"accounts/fireworks/models/z","displayName":"Z","conversationConfig":{},"supportsServerless":true,"supportsTools":true},
		{"name":"accounts/fireworks/models/not-chat","displayName":"No chat","supportsServerless":true},
		{"name":"accounts/fireworks/models/deployed","displayName":"Deployed","conversationConfig":{},"supportsServerless":false}
	],"nextPageToken":"page-2"}`
	if request.URL.Query().Get("pageToken") == "page-2" {
		body = `{"models":[
			{"name":"accounts/fireworks/models/a","displayName":"A","conversationConfig":{},"supportsServerless":true,"supportsTools":false},
			{"name":"accounts/fireworks/models/unknown","displayName":"Unknown","conversationConfig":{},"supportsServerless":true}
		]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func TestCatalogFollowsEveryPageAndUsesAttestedCapabilities(t *testing.T) {
	t.Setenv("ALT_FIREWORKS_API_KEY", "secret")
	transport := &pagedTransport{}
	factory := NewFactory(credential.NewStore(t.TempDir()))
	factory.HTTPClient = &http.Client{Transport: transport}

	models, err := factory.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(transport.requests))
	}
	if got := transport.requests[0].URL.String(); got != CatalogEndpoint {
		t.Fatalf("first URL = %q", got)
	}
	if got := transport.requests[1].URL.Query().Get("pageToken"); got != "page-2" {
		t.Fatalf("second page token = %q", got)
	}
	if len(models) != 3 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ID != "accounts/fireworks/models/a" ||
		models[0].Capabilities.ToolCalling != provider.CapabilityUnsupported {
		t.Fatalf("first model = %#v", models[0])
	}
	if models[1].Capabilities.ToolCalling != provider.CapabilityUnknown {
		t.Fatalf("unknown model capability = %#v", models[1].Capabilities)
	}
	if models[2].Capabilities.ToolCalling != provider.CapabilitySupported {
		t.Fatalf("last model capability = %#v", models[2].Capabilities)
	}
	for _, request := range transport.requests {
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
	}
}
