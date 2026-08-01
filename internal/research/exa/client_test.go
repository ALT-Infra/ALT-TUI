package exa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchUsesCurrentOfficialContractWithoutExposingCredential(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "secret" {
			t.Fatalf("x-api-key = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"url":"https://example.test","text":"evidence"}]}`))
	}))
	defer server.Close()
	client := Client{
		BaseURL: server.URL,
		ResolveCredential: func() (string, error) {
			return "secret", nil
		},
	}
	response, err := client.Search(context.Background(), SearchRequest{
		Query:      "primary source",
		NumResults: 3,
		Contents:   map[string]any{"text": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured["query"] != "primary source" || captured["numResults"] != float64(3) {
		t.Fatalf("payload = %#v", captured)
	}
	contents, ok := captured["contents"].(map[string]any)
	if !ok || contents["text"] != true {
		t.Fatalf("contents = %#v", captured["contents"])
	}
	results, ok := response["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("response = %#v", response)
	}
}

func TestContentsRetrievesExactURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var captured map[string]any
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		if request.URL.Path != "/contents" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		urls, ok := captured["urls"].([]any)
		if !ok || len(urls) != 2 {
			t.Fatalf("payload = %#v", captured)
		}
		_, _ = writer.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := Client{
		BaseURL: server.URL,
		ResolveCredential: func() (string, error) {
			return "secret", nil
		},
	}
	_, err := client.Contents(context.Background(), ContentsRequest{
		URLs: []string{"https://one.test", "https://two.test"},
		Contents: map[string]any{
			"text": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}
