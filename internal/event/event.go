package event

import (
	"encoding/json"
	"fmt"
	"time"

	"altv1/internal/content"

	"github.com/google/uuid"
)

type Kind string

const (
	SessionCreated         Kind = "session.created"
	UserInstruction        Kind = "user.instruction"
	ProfilePinned          Kind = "profile.pinned"
	SessionRecovered       Kind = "session.recovered"
	ModelCallStarted       Kind = "model.call.started"
	ModelUsage             Kind = "model.usage"
	ModelCacheEpochStarted Kind = "model.cache.epoch.started"
	ContextViewCommitted   Kind = "context.view.committed"
	ContextAgentCompacted  Kind = "context.agent.compacted"
	LeadershipTransferred  Kind = "leadership.transferred"
	AgentTurnStarted       Kind = "agent.turn.started"
	AgentTurnCompleted     Kind = "agent.turn.completed"
	AgentDecision          Kind = "agent.decision"
	DelegationCreated      Kind = "delegation.created"
	DelegationStarted      Kind = "delegation.started"
	ToolCalled             Kind = "tool.called"
	ToolCompleted          Kind = "tool.completed"
	DelegationTextDelta    Kind = "delegation.text.delta"
	DelegationReasoning    Kind = "delegation.reasoning.delta"
	DelegationCompleted    Kind = "delegation.completed"
	DelegationFailed       Kind = "delegation.failed"
	DelegationCancelled    Kind = "delegation.cancelled"
	PeerTurnCreated        Kind = "peer.turn.created"
	PeerTurnStarted        Kind = "peer.turn.started"
	PeerTextDelta          Kind = "peer.text.delta"
	PeerReasoning          Kind = "peer.reasoning.delta"
	PeerTurnCompleted      Kind = "peer.turn.completed"
	PeerTurnFailed         Kind = "peer.turn.failed"
	PeerTurnCancelled      Kind = "peer.turn.cancelled"
	FinalStarted           Kind = "final.started"
	FinalTextDelta         Kind = "final.text.delta"
	FinalReasoning         Kind = "final.reasoning.delta"
	FinalCompleted         Kind = "final.completed"
	SessionFailed          Kind = "session.failed"
	SessionCancelled       Kind = "session.cancelled"
)

type Event struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	Sequence      int64           `json:"sequence"`
	At            time.Time       `json:"at"`
	Kind          Kind            `json:"kind"`
	Actor         string          `json:"actor,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Data          json.RawMessage `json:"data"`
}

type Draft struct {
	Kind          Kind
	Actor         string
	CorrelationID string
	CausationID   string
	Data          any
}

func (d Draft) Materialize(sessionID string, sequence int64, at time.Time) (Event, error) {
	data := json.RawMessage("{}")
	if d.Data != nil {
		encoded, err := json.Marshal(d.Data)
		if err != nil {
			return Event{}, fmt.Errorf("marshal %s event: %w", d.Kind, err)
		}
		data = encoded
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, fmt.Errorf("create event id: %w", err)
	}
	return Event{
		ID:            id.String(),
		SessionID:     sessionID,
		Sequence:      sequence,
		At:            at.UTC(),
		Kind:          d.Kind,
		Actor:         d.Actor,
		CorrelationID: d.CorrelationID,
		CausationID:   d.CausationID,
		Data:          data,
	}, nil
}

func Decode[T any](e Event) (T, error) {
	var value T
	if err := json.Unmarshal(e.Data, &value); err != nil {
		return value, fmt.Errorf("decode %s event %s: %w", e.Kind, e.ID, err)
	}
	return value, nil
}

type SessionCreatedData struct {
	Task  string        `json:"task"`
	Input content.Input `json:"input,omitempty"`
}

type UserInstructionData struct {
	Text  string        `json:"text"`
	Input content.Input `json:"input,omitempty"`
}

type ProfilePinnedData struct {
	ProfileID string `json:"profile_id"`
	Revision  int    `json:"revision"`
	Digest    string `json:"digest"`
}

type LeadershipTransferredData struct {
	FromAgentID string `json:"from_agent_id,omitempty"`
	ToAgentID   string `json:"to_agent_id"`
	Reason      string `json:"reason"`
}

type AgentTurnData struct {
	AgentID     string   `json:"agent_id"`
	Turn        int      `json:"turn"`
	SignalKinds []string `json:"signal_kinds,omitempty"`
	Assessment  string   `json:"assessment,omitempty"`
}

type AgentDecisionData struct {
	AgentID       string           `json:"agent_id"`
	Turn          int              `json:"turn"`
	Assessment    string           `json:"assessment"`
	Delegations   []DelegationSpec `json:"delegations,omitempty"`
	PeerTurns     []PeerTurnSpec   `json:"peer_turns,omitempty"`
	Cancellations []string         `json:"cancellations,omitempty"`
	HandoffTo     string           `json:"handoff_to,omitempty"`
}

type PeerTurnSpec struct {
	ID              string   `json:"id"`
	Key             string   `json:"key,omitempty"`
	CollaborationID string   `json:"collaboration_id"`
	PeerID          string   `json:"peer_id"`
	CallerID        string   `json:"caller_id"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context,omitempty"`
	Attachments     []string `json:"attachments,omitempty"`
	RequiredTools   []string `json:"required_tools,omitempty"`
	Round           int      `json:"round"`
}

type PeerTurnStartedData struct {
	PeerTurnID string `json:"peer_turn_id"`
	Attempt    int    `json:"attempt"`
}

type PeerTextDeltaData struct {
	PeerTurnID string `json:"peer_turn_id"`
	Text       string `json:"text"`
}

type PeerTurnCompletedData struct {
	PeerTurnID string   `json:"peer_turn_id"`
	Attempt    int      `json:"attempt"`
	Result     string   `json:"result"`
	Findings   []string `json:"findings,omitempty"`
	Risks      []string `json:"risks,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

type PeerTurnFailedData struct {
	PeerTurnID string `json:"peer_turn_id"`
	Attempt    int    `json:"attempt"`
	Error      string `json:"error"`
}

type PeerTurnCancelledData struct {
	PeerTurnID string `json:"peer_turn_id"`
	Reason     string `json:"reason"`
}

type DelegationSpec struct {
	ID            string   `json:"id"`
	Key           string   `json:"key,omitempty"`
	SpecialistID  string   `json:"specialist_id"`
	CallerID      string   `json:"caller_id"`
	Objective     string   `json:"objective"`
	Context       string   `json:"context,omitempty"`
	Attachments   []string `json:"attachments,omitempty"`
	DependsOn     []string `json:"depends_on,omitempty"`
	RequiredTools []string `json:"required_tools,omitempty"`
	Depth         int      `json:"depth"`
}

type DelegationStartedData struct {
	DelegationID string `json:"delegation_id"`
	Attempt      int    `json:"attempt"`
}

type TextDeltaData struct {
	DelegationID string `json:"delegation_id,omitempty"`
	Text         string `json:"text"`
}

type ToolCallData struct {
	DelegationID string `json:"delegation_id"`
	ToolCallID   string `json:"tool_call_id"`
	Tool         string `json:"tool"`
	Provider     string `json:"provider,omitempty"`
	Arguments    string `json:"arguments,omitempty"`
}

type ToolCompletedData struct {
	DelegationID string `json:"delegation_id"`
	ToolCallID   string `json:"tool_call_id"`
	Tool         string `json:"tool"`
	Failed       bool   `json:"failed,omitempty"`
	Error        string `json:"error,omitempty"`
	Result       string `json:"result,omitempty"`
}

type DelegationCompletedData struct {
	DelegationID string   `json:"delegation_id"`
	Attempt      int      `json:"attempt"`
	Result       string   `json:"result"`
	Findings     []string `json:"findings,omitempty"`
	Risks        []string `json:"risks,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
}

type DelegationFailedData struct {
	DelegationID string `json:"delegation_id"`
	Attempt      int    `json:"attempt"`
	Error        string `json:"error"`
}

type DelegationCancelledData struct {
	DelegationID string `json:"delegation_id"`
	Reason       string `json:"reason"`
}

type FinalCompletedData struct {
	Answer string `json:"answer"`
}

type FailureData struct {
	Error string `json:"error"`
}

type ModelUsageData struct {
	Model                string `json:"model"`
	Purpose              string `json:"purpose"`
	PromptTokens         int    `json:"prompt_tokens"`
	UncachedPromptTokens int    `json:"uncached_prompt_tokens,omitempty"`
	CachedPromptTokens   int    `json:"cached_prompt_tokens,omitempty"`
	CacheWriteTokens     int    `json:"cache_write_tokens,omitempty"`
	CacheMissTokens      int    `json:"cache_miss_tokens,omitempty"`
	CacheUsageReported   bool   `json:"cache_usage_reported,omitempty"`
	CompletionTokens     int    `json:"completion_tokens"`
	ReasoningTokens      int    `json:"reasoning_tokens"`
	TotalTokens          int    `json:"total_tokens"`
}

type ModelCallStartedData struct {
	Model   string `json:"model"`
	Purpose string `json:"purpose"`
}

type ModelCacheEpochStartedData struct {
	AgentID           string   `json:"agent_id"`
	Epoch             int      `json:"epoch"`
	Reason            string   `json:"reason"`
	HeaderDigest      string   `json:"header_digest"`
	ToolNames         []string `json:"tool_names,omitempty"`
	DeferredToolNames []string `json:"deferred_tool_names,omitempty"`
}

type ContextViewCommittedData struct {
	ScopeKind             string `json:"scope_kind"`
	ScopeID               string `json:"scope_id"`
	Epoch                 int    `json:"epoch"`
	SourceThroughSequence int64  `json:"source_through_sequence"`
	ViewDigest            string `json:"view_digest"`
	EstimatedTokens       int    `json:"estimated_tokens"`
	Compacted             bool   `json:"compacted"`
}

type ContextAgentCompactedData struct {
	Scope               string `json:"scope"`
	Trigger             string `json:"trigger,omitempty"`
	TranscriptReference string `json:"transcript_reference"`
	MessagesBefore      int    `json:"messages_before"`
	MessagesAfter       int    `json:"messages_after"`
	EstimatedTokens     int    `json:"estimated_tokens,omitempty"`
	PromptCapacity      int    `json:"prompt_capacity,omitempty"`
	HighWater           int    `json:"high_water,omitempty"`
}
