package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestCacheUsageFromJSONNormalizesGatewayShapes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want CacheUsage
	}{
		{"openai nested", `{"usage":{"prompt_tokens_details":{"cached_tokens":72}}}`, CacheUsage{Reported: true, ReadTokens: 72}},
		{"deepseek native", `{"usage":{"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":8}}`, CacheUsage{Reported: true, ReadTokens: 64, MissTokens: 8}},
		{"together top level", `{"usage":{"cached_tokens":51}}`, CacheUsage{Reported: true, ReadTokens: 51}},
		{"anthropic gateway", `{"usage":{"cache_read_input_tokens":40,"cache_creation_input_tokens":12}}`, CacheUsage{Reported: true, ReadTokens: 40, WriteTokens: 12}},
		{"reported miss", `{"usage":{"prompt_tokens_details":{"cached_tokens":0}}}`, CacheUsage{Reported: true}},
		{"not reported", `{"usage":{"prompt_tokens":10}}`, CacheUsage{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cacheUsageFromJSON([]byte(test.raw)); got != test.want {
				t.Fatalf("cache usage = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestHTTPAvailabilitySignalSurvivesTheGatewayAdapter(t *testing.T) {
	client := CacheAwareHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header: http.Header{
					"Content-Type": {"application/json"},
					"Retry-After":  {"7"},
				},
				Body:    io.NopCloser(strings.NewReader(`{"error":{"message":"temporarily unavailable"}}`)),
				Request: request,
			}, nil
		}),
	})
	wrapped := ObserveCacheUsage(&httpFailingModel{client: client})
	_, err := wrapped.Generate(context.Background(), []*schema.Message{schema.UserMessage("continue")})
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("gateway error was not normalized: %T %v", err, err)
	}
	if failure.Kind != FailureTransient || failure.RetryAfter != 7*time.Second {
		t.Fatalf("availability signal = %#v", failure)
	}
}

type httpFailingModel struct {
	client *http.Client
}

func (m *httpFailingModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://gateway.test/chat/completions", nil)
	response, err := m.client.Do(request)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return nil, errors.New("gateway temporarily unavailable")
}

func (m *httpFailingModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}

func TestCacheAwareHTTPClientObservesStreamingUsageWithoutChangingBytes(t *testing.T) {
	body := "data: {\"choices\":[]}\n\n" +
		"data: {\"usage\":{\"cached_tokens\":33}}\n\n" +
		"data: [DONE]\n\n"
	base := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	tracker := &cacheTracker{}
	request, _ := http.NewRequestWithContext(context.WithValue(context.Background(), cacheTrackerKey{}, tracker), http.MethodPost, "https://example.test", nil)
	response, err := CacheAwareHTTPClient(base).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, []byte(body)) {
		t.Fatalf("stream was modified:\n%s", gotBody)
	}
	if got := tracker.snapshot(); got != (CacheUsage{Reported: true, ReadTokens: 33}) {
		t.Fatalf("cache usage = %#v", got)
	}
}

func TestOrdinaryJSONResponseIsObservedWithoutPrebufferingTheCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	base := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       reader,
			Request:    request,
		}, nil
	})}
	tracker := &cacheTracker{}
	request, _ := http.NewRequestWithContext(
		context.WithValue(context.Background(), cacheTrackerKey{}, tracker),
		http.MethodPost, "https://example.test", nil,
	)
	type result struct {
		response *http.Response
		err      error
	}
	returned := make(chan result, 1)
	go func() {
		response, err := CacheAwareHTTPClient(base).Do(request)
		returned <- result{response: response, err: err}
	}()
	var response *http.Response
	select {
	case call := <-returned:
		if call.err != nil {
			t.Fatal(call.err)
		}
		response = call.response
	case <-time.After(time.Second):
		t.Fatal("HTTP client buffered the response before returning headers")
	}

	payload := `{"choices":[{"message":{"content":"` + strings.Repeat("large completion ", 20_000) + `"}}],"usage":{"cached_tokens":91}}`
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, payload)
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	observed, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := <-writeDone; writeErr != nil {
		t.Fatal(writeErr)
	}
	if !bytes.Equal(observed, []byte(payload)) {
		t.Fatal("cache observation changed ordinary JSON response bytes")
	}
	if got := tracker.snapshot(); got != (CacheUsage{Reported: true, ReadTokens: 91}) {
		t.Fatalf("streamed JSON cache usage = %#v", got)
	}
}

func TestObservedCacheUsageSurvivesEinoMessageConcatenation(t *testing.T) {
	base := &streamingCacheModel{}
	stream, err := ObserveCacheUsage(base).Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []*schema.Message
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, message)
	}
	combined, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if got := CacheUsageFromMessage(combined); got != (CacheUsage{Reported: true, ReadTokens: 21, WriteTokens: 8, MissTokens: 13}) {
		t.Fatalf("cache usage = %#v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type streamingCacheModel struct{}

func (*streamingCacheModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (*streamingCacheModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	tracker, _ := ctx.Value(cacheTrackerKey{}).(*cacheTracker)
	tracker.merge(CacheUsage{Reported: true, ReadTokens: 21, WriteTokens: 8, MissTokens: 13})
	final := schema.AssistantMessage("k", nil)
	final.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 30}}
	return schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("o", nil), final,
	}), nil
}
