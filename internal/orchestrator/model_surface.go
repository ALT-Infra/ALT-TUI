package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"altv1/internal/event"
	"altv1/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

const modelSurfaceVersion = 1

// modelSurface is the exact, versioned conversation tail that a context-bearing
// Team agent has seen. It deliberately excludes specialists: their contract is
// a clean slate on every invocation.
type modelSurface struct {
	Version            int               `json:"version"`
	ConversationID     string            `json:"conversation_id"`
	AgentID            string            `json:"agent_id"`
	Epoch              int               `json:"epoch"`
	HeaderDigest       string            `json:"header_digest,omitempty"`
	Messages           []*schema.Message `json:"messages"`
	LastSessionID      string            `json:"last_session_id,omitempty"`
	LastSnapshotDigest string            `json:"last_snapshot_digest,omitempty"`
	CommitDigest       string            `json:"commit_digest,omitempty"`
}

func modelSurfaceKey(conversationID, agentID string) string {
	digest := sha256.Sum256([]byte(conversationID + "\x00" + agentID))
	return "alt:model-surface:v1:" + hex.EncodeToString(digest[:])
}

func loadModelSurface(ctx context.Context, ledger *store.Store, conversationID, agentID string) (*modelSurface, error) {
	raw, exists, err := ledger.Get(ctx, modelSurfaceKey(conversationID, agentID))
	if err != nil {
		return nil, err
	}
	if !exists {
		return &modelSurface{Version: modelSurfaceVersion, ConversationID: conversationID, AgentID: agentID}, nil
	}
	var surface modelSurface
	if err := json.Unmarshal(raw, &surface); err != nil {
		return nil, fmt.Errorf("decode model surface for %s: %w", agentID, err)
	}
	if surface.Version != modelSurfaceVersion || surface.ConversationID != conversationID || surface.AgentID != agentID {
		return nil, fmt.Errorf("model surface identity mismatch for %s", agentID)
	}
	return &surface, nil
}

func saveModelSurface(ctx context.Context, ledger *store.Store, surface *modelSurface) error {
	if surface == nil {
		return nil
	}
	raw, err := json.Marshal(surface)
	if err != nil {
		return fmt.Errorf("encode model surface for %s: %w", surface.AgentID, err)
	}
	return ledger.Set(context.WithoutCancel(ctx), modelSurfaceKey(surface.ConversationID, surface.AgentID), raw)
}

func (r *sessionRuntime) lockModelSurface(agentID string) func() {
	r.surfaceLocksMu.Lock()
	if r.surfaceLocks == nil {
		r.surfaceLocks = make(map[string]*sync.Mutex)
	}
	lock := r.surfaceLocks[agentID]
	if lock == nil {
		lock = &sync.Mutex{}
		r.surfaceLocks[agentID] = lock
	}
	r.surfaceLocksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func prepareModelSurfaceMessages(
	surface *modelSurface,
	system string,
	sessionID string,
	exactUserMessage *schema.Message,
	snapshot string,
) ([]*schema.Message, string, error) {
	snapshotDigest := digestText(snapshot)
	return prepareModelSurfaceMessagesWithSnapshot(
		surface, system, sessionID, exactUserMessage,
		schema.UserMessage(runtimeSnapshotText(snapshotDigest, snapshot)), snapshotDigest,
	)
}

func prepareModelSurfaceMessagesWithSnapshot(
	surface *modelSurface,
	system string,
	sessionID string,
	exactUserMessage *schema.Message,
	snapshotMessage *schema.Message,
	snapshotDigest string,
) ([]*schema.Message, string, error) {
	messages := cloneModelMessages(surface.Messages)
	if len(messages) > 0 {
		if messages[0] != nil && messages[0].Role == schema.System {
			messages = messages[1:]
		}
	}
	if surface.LastSessionID != sessionID {
		if exactUserMessage == nil {
			return nil, "", fmt.Errorf("exact user message is required for a new user turn")
		}
		messages = append(messages, cloneModelMessage(exactUserMessage))
	}
	if snapshotDigest != surface.LastSnapshotDigest {
		if snapshotMessage == nil {
			return nil, "", fmt.Errorf("runtime snapshot message is required when the snapshot changes")
		}
		messages = append(messages, cloneModelMessage(snapshotMessage))
	}
	return messages, snapshotDigest, nil
}

func runtimeSnapshotText(digest, snapshot string) string {
	return "ALT RUNTIME SNAPSHOT " + digest + "\n" +
		"This durable snapshot supersedes earlier ALT runtime snapshots where they conflict. " +
		"It is supporting state, not a replacement or paraphrase of the user's exact message.\n\n" + snapshot
}

type modelSurfaceHandler struct {
	*adk.BaseChatModelAgentMiddleware
	ledger         *store.Store
	sessionID      string
	modelIdentity  string
	surface        *modelSurface
	snapshotDigest string
}

func newModelSurfaceHandler(
	ledger *store.Store,
	sessionID string,
	modelIdentity string,
	surface *modelSurface,
	snapshotDigest string,
) adk.ChatModelAgentMiddleware {
	return &modelSurfaceHandler{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		ledger:                       ledger, sessionID: sessionID, modelIdentity: modelIdentity,
		surface: surface, snapshotDigest: snapshotDigest,
	}
}

func (h *modelSurfaceHandler) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil {
		return ctx, state, nil
	}
	state.ToolInfos = canonicalToolInfos(state.ToolInfos)
	headerDigest := digestModelHeader(h.modelIdentity, state.Messages, state.ToolInfos, state.DeferredToolInfos)
	prefixStable := modelMessagesHavePrefix(state.Messages, h.surface.Messages)
	reason := ""
	if h.surface.Epoch == 0 {
		h.surface.Epoch = 1
		reason = "initial canonical request header"
	} else {
		var changes []string
		if h.surface.HeaderDigest != "" && h.surface.HeaderDigest != headerDigest {
			changes = append(changes, "request header changed")
		}
		if len(h.surface.Messages) > 0 && !prefixStable {
			changes = append(changes, "message prefix replaced")
		}
		if len(changes) > 0 {
			h.surface.Epoch++
			reason = strings.Join(changes, "; ")
		}
	}
	if reason != "" {
		if _, err := h.ledger.Append(ctx, h.sessionID, event.Draft{
			Kind: event.ModelCacheEpochStarted, Actor: h.surface.AgentID,
			Data: event.ModelCacheEpochStartedData{
				AgentID: h.surface.AgentID, Epoch: h.surface.Epoch,
				Reason: reason, HeaderDigest: headerDigest,
				ToolNames:         toolInfoNames(state.ToolInfos),
				DeferredToolNames: toolInfoNames(state.DeferredToolInfos),
			},
		}); err != nil {
			return ctx, nil, err
		}
	}
	h.surface.HeaderDigest = headerDigest
	return ctx, state, nil
}

func toolInfoNames(values []*schema.ToolInfo) []string {
	canonical := canonicalToolInfos(values)
	result := make([]string, 0, len(canonical))
	for _, value := range canonical {
		if value != nil {
			result = append(result, value.Name)
		}
	}
	return result
}

func (h *modelSurfaceHandler) AfterModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if err := h.commit(ctx, state); err != nil {
		return ctx, nil, err
	}
	return ctx, state, nil
}

func (h *modelSurfaceHandler) AfterAgent(ctx context.Context, state *adk.ChatModelAgentState) (context.Context, error) {
	return ctx, h.commit(ctx, state)
}

func (h *modelSurfaceHandler) commit(ctx context.Context, state *adk.ChatModelAgentState) error {
	if state == nil {
		return nil
	}
	h.surface.Messages = cloneModelMessages(state.Messages)
	h.surface.LastSessionID = h.sessionID
	h.surface.LastSnapshotDigest = h.snapshotDigest
	digest, err := modelSurfaceCommitDigest(h.surface)
	if err != nil {
		return err
	}
	if digest == h.surface.CommitDigest {
		return nil
	}
	h.surface.CommitDigest = digest
	return saveModelSurface(ctx, h.ledger, h.surface)
}

func modelSurfaceCommitDigest(surface *modelSurface) (string, error) {
	if surface == nil {
		return "", nil
	}
	copy := *surface
	copy.CommitDigest = ""
	raw, err := json.Marshal(&copy)
	if err != nil {
		return "", fmt.Errorf("digest model surface for %s: %w", surface.AgentID, err)
	}
	return digestBytes(raw), nil
}

func canonicalToolInfos(values []*schema.ToolInfo) []*schema.ToolInfo {
	result := append([]*schema.ToolInfo(nil), values...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i] == nil {
			return result[j] != nil
		}
		if result[j] == nil {
			return false
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func digestModelHeader(modelIdentity string, messages []*schema.Message, tools, deferred []*schema.ToolInfo) string {
	system := ""
	if len(messages) > 0 && messages[0] != nil && messages[0].Role == schema.System {
		system = messages[0].Content
	}
	payload, _ := json.Marshal(struct {
		Model    string                `json:"model"`
		System   string                `json:"system"`
		Tools    []canonicalToolHeader `json:"tools"`
		Deferred []canonicalToolHeader `json:"deferred_tools"`
	}{modelIdentity, system, canonicalToolHeaders(tools), canonicalToolHeaders(deferred)})
	return digestBytes(payload)
}

// canonicalToolHeader matches the cache-bearing semantics of Eino's OpenAI
// adapter instead of hashing its mutable in-memory ToolInfo representation.
// That adapter sorts JSON-schema required arrays in place before serializing;
// canonicalizing them here keeps an equivalent wire header in one cache epoch.
type canonicalToolHeader struct {
	Name   string         `json:"name"`
	Desc   string         `json:"description,omitempty"`
	Extra  map[string]any `json:"extra,omitempty"`
	Params any            `json:"parameters,omitempty"`
}

func canonicalToolHeaders(values []*schema.ToolInfo) []canonicalToolHeader {
	values = canonicalToolInfos(values)
	result := make([]canonicalToolHeader, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		header := canonicalToolHeader{Name: value.Name, Desc: value.Desc, Extra: value.Extra}
		if value.ParamsOneOf != nil {
			parameters, err := value.ParamsOneOf.ToJSONSchema()
			if err != nil {
				header.Params = map[string]any{"alt_schema_error": err.Error()}
			} else if raw, marshalErr := json.Marshal(parameters); marshalErr == nil {
				var generic any
				if json.Unmarshal(raw, &generic) == nil {
					canonicalizeRequiredArrays(generic)
					header.Params = generic
				}
			}
		}
		result = append(result, header)
	}
	return result
}

func canonicalizeRequiredArrays(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "required" {
				if required, ok := child.([]any); ok {
					sort.SliceStable(required, func(i, j int) bool {
						left, _ := required[i].(string)
						right, _ := required[j].(string)
						return left < right
					})
				}
			}
			canonicalizeRequiredArrays(child)
		}
	case []any:
		for _, child := range typed {
			canonicalizeRequiredArrays(child)
		}
	}
}

func modelMessagesHavePrefix(messages, prefix []*schema.Message) bool {
	if len(prefix) == 0 {
		return true
	}
	if len(messages) < len(prefix) {
		return false
	}
	left, _ := json.Marshal(sanitizeModelMessages(cloneModelMessages(messages[:len(prefix)])))
	right, _ := json.Marshal(sanitizeModelMessages(cloneModelMessages(prefix)))
	return string(left) == string(right)
}

func cloneModelMessages(values []*schema.Message) []*schema.Message {
	if len(values) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(values)
	var result []*schema.Message
	if json.Unmarshal(encoded, &result) != nil {
		result = append([]*schema.Message(nil), values...)
	}
	return sanitizeModelMessages(result)
}

func cloneModelMessage(value *schema.Message) *schema.Message {
	if value == nil {
		return nil
	}
	values := cloneModelMessages([]*schema.Message{value})
	return values[0]
}

func sanitizeModelMessages(values []*schema.Message) []*schema.Message {
	for _, message := range values {
		if message == nil {
			continue
		}
		message.ResponseMeta = nil
		if message.Extra != nil {
			delete(message.Extra, "alt.cache_usage")
			if len(message.Extra) == 0 {
				message.Extra = nil
			}
		}
	}
	return values
}

func digestText(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
