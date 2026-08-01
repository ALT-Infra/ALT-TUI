package together

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"altv1/internal/credential"
)

type catalogTransport struct {
	request *http.Request
	body    string
}

func (c *catalogTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.request = request
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    request,
	}, nil
}

func TestCatalogKeepsOnlyChatCompletionModelTypes(t *testing.T) {
	t.Setenv("ALT_TOGETHER_API_KEY", "secret")
	transport := &catalogTransport{body: `[
		{"id":"org/chat","type":"chat","display_name":"Chat"},
		{"id":"org/code","type":"code","display_name":"Code"},
		{"id":"org/image","type":"image","display_name":"Image"},
		{"id":"org/chat","type":"chat","display_name":"duplicate"}
	]`}
	factory := NewFactory(credential.NewStore(t.TempDir()))
	factory.HTTPClient = &http.Client{Transport: transport}

	models, err := factory.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "org/chat" || models[1].ID != "org/code" {
		t.Fatalf("models = %#v", models)
	}
	if got := transport.request.URL.String(); got != DefaultEndpoint+"/models" {
		t.Fatalf("catalog URL = %q", got)
	}
	if got := transport.request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
