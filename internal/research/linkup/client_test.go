package linkup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchUsesCurrentLinkupContract(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("authorization") != "Bearer secret" {
			t.Fatalf("authorization header was not set")
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(`{"data":{"answer":"supported"}}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, ResolveCredential: func() (string, error) { return "secret", nil }}
	_, err := client.Search(context.Background(), SearchRequest{
		Query: "Find primary evidence", Depth: "deep", OutputType: "structured",
		IncludeDomains: []string{"example.org"}, IncludeSources: true,
		StructuredOutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"finding": map[string]any{"type": "string"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured["q"] != "Find primary evidence" || captured["depth"] != "deep" || captured["outputType"] != "structured" {
		t.Fatalf("search payload = %#v", captured)
	}
	if captured["includeSources"] != true {
		t.Fatalf("includeSources = %#v", captured["includeSources"])
	}
	serialized, ok := captured["structuredOutputSchema"].(string)
	if !ok {
		t.Fatalf("structuredOutputSchema = %#v", captured["structuredOutputSchema"])
	}
	var schema map[string]any
	if json.Unmarshal([]byte(serialized), &schema) != nil || schema["type"] != "object" {
		t.Fatalf("serialized schema = %q", serialized)
	}
}

func TestFetchExposesRenderingAndExtraction(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/fetch" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(`{"markdown":"evidence","favicon":""}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, ResolveCredential: func() (string, error) { return "secret", nil }}
	_, err := client.Fetch(context.Background(), FetchRequest{
		URL: "https://example.org", RenderJS: true, IncludeRawContent: true, ExtractImages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured["renderJs"] != true || captured["includeRawContent"] != true || captured["extractImages"] != true {
		t.Fatalf("fetch payload = %#v", captured)
	}
}

func TestPermanentErrorMessageSurvives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusPaymentRequired)
		_, _ = writer.Write([]byte(`{"error":{"code":"INSUFFICIENT_FUNDS_CREDITS","message":"Insufficient funds"}}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, ResolveCredential: func() (string, error) { return "secret", nil }}
	_, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org"})
	if err == nil || err.Error() != "Linkup returned HTTP 402: Insufficient funds" {
		t.Fatalf("error = %v", err)
	}
}
