package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExplicitCacheControlMarksStableSystemBoundary(t *testing.T) {
	var captured map[string]any
	client := ExplicitCacheControlHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &captured); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request,
		}, nil
	})})
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/chat/completions", strings.NewReader(`{
		"model":"anthropic/example",
		"messages":[
			{"role":"system","content":"stable role and tools"},
			{"role":"user","content":"variable question"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	messages := captured["messages"].([]any)
	system := messages[0].(map[string]any)
	blocks := system["content"].([]any)
	block := blocks[0].(map[string]any)
	control := block["cache_control"].(map[string]any)
	if block["text"] != "stable role and tools" || control["type"] != "ephemeral" {
		t.Fatalf("system cache breakpoint = %#v", block)
	}
	if messages[1].(map[string]any)["content"] != "variable question" {
		t.Fatal("explicit cache adapter changed variable user content")
	}
}

func TestExplicitCacheControlStreamsPastTheStablePrefixBeforeLargeUserContentArrives(t *testing.T) {
	prefixObserved := make(chan struct{})
	var captured []byte
	client := ExplicitCacheControlHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		probe := make([]byte, 64)
		n, err := request.Body.Read(probe)
		if err != nil && err != io.EOF {
			return nil, err
		}
		close(prefixObserved)
		rest, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		captured = append(append([]byte(nil), probe[:n]...), rest...)
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request,
		}, nil
	})})
	source, writer := io.Pipe()
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/chat/completions", source)
	request.Header.Set("Content-Type", "application/json")
	returned := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if err == nil {
			err = response.Body.Close()
		}
		returned <- err
	}()
	prefix := `{"model":"example","messages":[{"role":"system","content":"stable role"},{"role":"user","content":"`
	prefixWritten := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, prefix)
		prefixWritten <- err
	}()
	select {
	case <-prefixObserved:
	case <-time.After(time.Second):
		t.Fatal("cache-control transport waited for the large user body before sending the stable prefix")
	}
	if err := <-prefixWritten; err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("x"), 2<<20)
	if _, err := writer.Write(append(large, []byte(`"}]}`)...)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(captured, &decoded); err != nil {
		t.Fatal(err)
	}
	messages := decoded["messages"].([]any)
	systemBlocks := messages[0].(map[string]any)["content"].([]any)
	if systemBlocks[0].(map[string]any)["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatal("streaming rewrite lost the cache breakpoint")
	}
	if len(messages[1].(map[string]any)["content"].(string)) != len(large) {
		t.Fatal("streaming rewrite changed the large user content")
	}
}
