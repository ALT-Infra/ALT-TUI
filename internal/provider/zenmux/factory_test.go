package zenmux

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

func TestCatalogUsesExactTextModelIdentities(t *testing.T) {
	t.Setenv("ALT_ZENMUX_API_KEY", "secret")
	transport := &catalogTransport{body: `{"data":[
		{"id":"openai/gpt-test","display_name":"GPT Test","output_modalities":["text"]},
		{"id":"image/only","display_name":"Image","output_modalities":["image"]},
		{"id":"openai/gpt-test","display_name":"duplicate","output_modalities":["text"]}
	]}`}
	factory := NewFactory(credential.NewStore(t.TempDir()))
	factory.HTTPClient = &http.Client{Transport: transport}

	models, err := factory.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "openai/gpt-test" ||
		models[0].DisplayName != "GPT Test" {
		t.Fatalf("models = %#v", models)
	}
	if got := transport.request.URL.String(); got != DefaultEndpoint+"/models" {
		t.Fatalf("catalog URL = %q", got)
	}
	if got := transport.request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
