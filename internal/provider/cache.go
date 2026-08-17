package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const cacheUsageExtraKey = "alt.cache_usage"

// CacheUsage is ALT's provider-neutral view of prompt-cache accounting. A
// reported zero is different from an absent field: the former proves that the
// gateway supports telemetry and that this request missed, while the latter
// remains honestly unknown.
type CacheUsage struct {
	Reported    bool `json:"reported"`
	ReadTokens  int  `json:"read_tokens,omitempty"`
	WriteTokens int  `json:"write_tokens,omitempty"`
	MissTokens  int  `json:"miss_tokens,omitempty"`
}

type cacheTrackerKey struct{}
type cacheScopeKey struct{}

// WithCacheScope identifies a durable model-visible conversation without
// prescribing a gateway mechanism. Adapters with an affinity facility may use
// it; other gateways safely ignore it.
func WithCacheScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, cacheScopeKey{}, strings.TrimSpace(scope))
}

// CacheAffinityKey returns a non-sensitive stable identifier suitable for
// gateway routing fields such as Fireworks' prompt_cache_key.
func CacheAffinityKey(ctx context.Context) string {
	scope, _ := ctx.Value(cacheScopeKey{}).(string)
	if scope == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(scope))
	return "alt-" + hex.EncodeToString(digest[:16])
}

type cacheTracker struct {
	mu             sync.Mutex
	usage          CacheUsage
	responseStatus int
	retryAfter     time.Duration
}

func (t *cacheTracker) merge(value CacheUsage) {
	if t == nil || !value.Reported {
		return
	}
	t.mu.Lock()
	t.usage.Reported = true
	t.usage.ReadTokens = max(t.usage.ReadTokens, value.ReadTokens)
	t.usage.WriteTokens = max(t.usage.WriteTokens, value.WriteTokens)
	t.usage.MissTokens = max(t.usage.MissTokens, value.MissTokens)
	t.mu.Unlock()
}

func (t *cacheTracker) snapshot() CacheUsage {
	if t == nil {
		return CacheUsage{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage
}

func (t *cacheTracker) observeResponse(response *http.Response) {
	if t == nil || response == nil {
		return
	}
	t.mu.Lock()
	t.responseStatus = response.StatusCode
	t.retryAfter = parseRetryAfter(response)
	t.mu.Unlock()
}

func (t *cacheTracker) normalize(err error) error {
	if t == nil || err == nil {
		return NormalizeFailure(err)
	}
	t.mu.Lock()
	status, retryAfter := t.responseStatus, t.retryAfter
	t.mu.Unlock()
	return normalizeFailureWithHint(err, status, retryAfter)
}

func parseRetryAfter(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	raw := strings.TrimSpace(response.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	availableAt, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	base := time.Now()
	if serverDate, dateErr := http.ParseTime(response.Header.Get("Date")); dateErr == nil {
		base = serverDate
	}
	if !availableAt.After(base) {
		return 0
	}
	return availableAt.Sub(base)
}

// CacheAwareHTTPClient returns a shallow client copy whose transport observes
// cache accounting in both ordinary JSON and streaming SSE responses. The
// response bytes are never rewritten, so this cannot change gateway protocol
// semantics or streaming latency.
func CacheAwareHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	clone := *base
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, ok := transport.(*cacheUsageTransport); !ok {
		clone.Transport = &cacheUsageTransport{base: transport}
	}
	return &clone
}

type cacheUsageTransport struct {
	base http.RoundTripper
}

func (t *cacheUsageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	tracker, _ := request.Context().Value(cacheTrackerKey{}).(*cacheTracker)
	if tracker == nil {
		return response, nil
	}
	tracker.observeResponse(response)
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = &cacheUsageSSEBody{source: response.Body, reader: bufio.NewReader(response.Body), tracker: tracker}
		return response, nil
	}
	response.Body = newCacheUsageJSONBody(response.Body, tracker)
	return response, nil
}

// cacheUsageJSONBody mirrors bytes into a streaming JSON decoder while the
// gateway SDK consumes the original response. Unknown response fields are
// skipped by encoding/json rather than buffered, so a large completion never
// becomes a second in-memory copy merely to recover a few usage counters.
type cacheUsageJSONBody struct {
	source io.ReadCloser
	writer *io.PipeWriter
	done   chan struct{}
	once   sync.Once
}

func newCacheUsageJSONBody(source io.ReadCloser, tracker *cacheTracker) *cacheUsageJSONBody {
	reader, writer := io.Pipe()
	body := &cacheUsageJSONBody{source: source, writer: writer, done: make(chan struct{})}
	go func() {
		defer close(body.done)
		defer reader.Close()
		var envelope cacheUsageEnvelope
		if json.NewDecoder(reader).Decode(&envelope) == nil {
			tracker.merge(envelope.normalized())
		}
		// A malformed or prematurely closed telemetry parse must never block
		// the real response reader.
		_, _ = io.Copy(io.Discard, reader)
	}()
	return body
}

func (b *cacheUsageJSONBody) Read(target []byte) (int, error) {
	n, err := b.source.Read(target)
	if n > 0 {
		_, _ = b.writer.Write(target[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *cacheUsageJSONBody) Close() error {
	err := b.source.Close()
	b.finish()
	return err
}

func (b *cacheUsageJSONBody) finish() {
	b.once.Do(func() {
		_ = b.writer.Close()
		<-b.done
	})
}

type cacheUsageEnvelope struct {
	Usage    cacheUsageFields `json:"usage"`
	Response struct {
		Usage cacheUsageFields `json:"usage"`
	} `json:"response"`
}

type cacheUsageFields struct {
	PromptCacheHitTokens     *int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    *int `json:"prompt_cache_miss_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens    *int `json:"cache_write_input_tokens"`
	CacheWriteTokens         *int `json:"cache_write_tokens"`
	CachedTokens             *int `json:"cached_tokens"`
	PromptTokensDetails      struct {
		CachedTokens     *int `json:"cached_tokens"`
		CacheWriteTokens *int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (e cacheUsageEnvelope) normalized() CacheUsage {
	result := e.Usage.normalized()
	nested := e.Response.Usage.normalized()
	if nested.Reported {
		result.Reported = true
		result.ReadTokens = max(result.ReadTokens, nested.ReadTokens)
		result.WriteTokens = max(result.WriteTokens, nested.WriteTokens)
		result.MissTokens = max(result.MissTokens, nested.MissTokens)
	}
	return result
}

func (u cacheUsageFields) normalized() CacheUsage {
	read := firstObservedInt(
		u.PromptCacheHitTokens, u.CacheReadInputTokens, u.CachedTokens,
		u.PromptTokensDetails.CachedTokens, u.InputTokensDetails.CachedTokens,
	)
	write := firstObservedInt(
		u.CacheCreationInputTokens, u.CacheWriteInputTokens, u.CacheWriteTokens,
		u.PromptTokensDetails.CacheWriteTokens,
	)
	miss := firstObservedInt(u.PromptCacheMissTokens)
	return CacheUsage{
		Reported:   read.present || write.present || miss.present,
		ReadTokens: read.value, WriteTokens: write.value, MissTokens: miss.value,
	}
}

type observedInt struct {
	value   int
	present bool
}

func firstObservedInt(values ...*int) observedInt {
	for _, value := range values {
		if value != nil {
			return observedInt{value: max(0, *value), present: true}
		}
	}
	return observedInt{}
}

type cacheUsageSSEBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	tracker *cacheTracker
	pending []byte
}

func (b *cacheUsageSSEBody) Read(target []byte) (int, error) {
	for len(b.pending) == 0 {
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if !bytes.Equal(payload, []byte("[DONE]")) {
					b.tracker.merge(cacheUsageFromJSON(payload))
				}
			}
			b.pending = line
		}
		if err != nil && len(b.pending) == 0 {
			return 0, err
		}
	}
	n := copy(target, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *cacheUsageSSEBody) Close() error { return b.source.Close() }

func cacheUsageFromJSON(payload []byte) CacheUsage {
	var root map[string]any
	if json.Unmarshal(payload, &root) != nil {
		return CacheUsage{}
	}
	usage := objectField(root, "usage")
	if usage == nil {
		usage = objectField(objectField(root, "response"), "usage")
	}
	if usage == nil {
		return CacheUsage{}
	}
	result := CacheUsage{}
	read, readPresent := firstNumericField(
		usage,
		[]string{"prompt_cache_hit_tokens"},
		[]string{"cache_read_input_tokens"},
		[]string{"cached_tokens"},
		[]string{"prompt_tokens_details", "cached_tokens"},
		[]string{"input_tokens_details", "cached_tokens"},
	)
	write, writePresent := firstNumericField(
		usage,
		[]string{"cache_creation_input_tokens"},
		[]string{"cache_write_input_tokens"},
		[]string{"cache_write_tokens"},
		[]string{"prompt_tokens_details", "cache_write_tokens"},
	)
	miss, missPresent := firstNumericField(usage, []string{"prompt_cache_miss_tokens"})
	result.Reported = readPresent || writePresent || missPresent
	result.ReadTokens, result.WriteTokens, result.MissTokens = read, write, miss
	return result
}

func objectField(object map[string]any, key string) map[string]any {
	if object == nil {
		return nil
	}
	value, _ := object[key].(map[string]any)
	return value
}

func firstNumericField(object map[string]any, paths ...[]string) (int, bool) {
	for _, path := range paths {
		var value any = object
		present := true
		for _, key := range path {
			current, ok := value.(map[string]any)
			if !ok {
				present = false
				break
			}
			value, present = current[key]
			if !present {
				break
			}
		}
		if !present {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return max(0, int(typed)), true
		case json.Number:
			parsed, err := strconv.Atoi(typed.String())
			return max(0, parsed), err == nil
		}
	}
	return 0, false
}

// ObserveCacheUsage attaches transport-observed accounting to returned Eino
// messages. This preserves fields that generic OpenAI-compatible translators
// otherwise discard, including DeepSeek's top-level hit/miss counters and
// Together's occasionally top-level cached_tokens value.
func ObserveCacheUsage(base model.BaseChatModel) model.BaseChatModel {
	if base == nil {
		return nil
	}
	return &cacheUsageModel{base: base}
}

type cacheUsageModel struct {
	base model.BaseChatModel
}

func (m *cacheUsageModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	toolModel, ok := m.base.(model.ToolCallingChatModel)
	if !ok {
		return nil, fmt.Errorf("cache-observed model does not support tool binding")
	}
	bound, err := toolModel.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &cacheUsageModel{base: bound}, nil
}

func (m *cacheUsageModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	tracker := &cacheTracker{}
	message, err := m.base.Generate(context.WithValue(ctx, cacheTrackerKey{}, tracker), input, options...)
	if message != nil {
		attachCacheUsage(message, tracker.snapshot())
	}
	return message, tracker.normalize(err)
}

func (m *cacheUsageModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	tracker := &cacheTracker{}
	stream, err := m.base.Stream(context.WithValue(ctx, cacheTrackerKey{}, tracker), input, options...)
	if err != nil {
		return nil, tracker.normalize(err)
	}
	output, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		defer stream.Close()
		for {
			message, receiveErr := stream.Recv()
			if errors.Is(receiveErr, io.EOF) {
				usage := tracker.snapshot()
				if usage.Reported {
					// Keep telemetry on one otherwise-empty terminal chunk. Eino can
					// concatenate this without colliding with provider Extra fields,
					// while all real content chunks remain available immediately.
					terminal := schema.AssistantMessage("", nil)
					attachCacheUsage(terminal, usage)
					writer.Send(terminal, nil)
				}
				return
			}
			if receiveErr != nil {
				writer.Send(nil, receiveErr)
				return
			}
			if writer.Send(message, nil) {
				return
			}
		}
	}()
	return output, nil
}

func attachCacheUsage(message *schema.Message, usage CacheUsage) {
	if message == nil || !usage.Reported {
		return
	}
	if message.Extra == nil {
		message.Extra = make(map[string]any)
	}
	message.Extra[cacheUsageExtraKey] = usage
}

func CacheUsageFromMessage(message *schema.Message) CacheUsage {
	if message == nil {
		return CacheUsage{}
	}
	if raw, ok := message.Extra[cacheUsageExtraKey]; ok {
		switch typed := raw.(type) {
		case CacheUsage:
			return typed
		case *CacheUsage:
			if typed != nil {
				return *typed
			}
		case map[string]any:
			encoded, _ := json.Marshal(typed)
			var decoded CacheUsage
			if json.Unmarshal(encoded, &decoded) == nil {
				return decoded
			}
		}
	}
	if message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
		cached := message.ResponseMeta.Usage.PromptTokenDetails.CachedTokens
		if cached > 0 {
			return CacheUsage{Reported: true, ReadTokens: cached}
		}
	}
	return CacheUsage{}
}
