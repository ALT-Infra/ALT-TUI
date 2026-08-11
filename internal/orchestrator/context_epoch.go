package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"altv1/internal/event"
	"altv1/internal/store"
)

type persistedPromptView struct {
	Prompt string `json:"prompt"`
}

func (r *sessionRuntime) commitWorkingView(
	ctx context.Context,
	scopeKind string,
	scopeID string,
	sourceThrough int64,
	prompt string,
) (store.ContextEpoch, error) {
	if sourceThrough < 1 {
		return store.ContextEpoch{}, fmt.Errorf("cannot commit %s context before its source is durable", scopeKind)
	}
	view, err := json.Marshal(persistedPromptView{Prompt: prompt})
	if err != nil {
		return store.ContextEpoch{}, fmt.Errorf("encode %s working view: %w", scopeKind, err)
	}
	epoch, err := r.store.CommitContextEpoch(ctx, store.ContextEpoch{
		SessionID: r.session.ID, ScopeKind: scopeKind, ScopeID: scopeID,
		SourceThroughSequence: sourceThrough, View: view,
		EstimatedTokens: (len(prompt) + 3) / 4,
	})
	if err != nil {
		return store.ContextEpoch{}, err
	}
	compacted := strings.Contains(prompt, `"compacted": true`) ||
		strings.Contains(prompt, `"archived_evidence": {`) ||
		strings.Contains(prompt, `"archived": {`) ||
		(strings.Contains(prompt, `"archived_rounds":`) &&
			!strings.Contains(prompt, `"archived_rounds": 0`))
	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.ContextViewCommitted, Actor: "context",
		CorrelationID: scopeKind + ":" + scopeID,
		Data: event.ContextViewCommittedData{
			ScopeKind: scopeKind, ScopeID: scopeID, Epoch: epoch.Epoch,
			SourceThroughSequence: epoch.SourceThroughSequence,
			ViewDigest:            epoch.ViewDigest, EstimatedTokens: epoch.EstimatedTokens,
			Compacted: compacted,
		},
	}); err != nil {
		return store.ContextEpoch{}, err
	}
	return epoch, nil
}

func (r *sessionRuntime) recordAgentCompaction(
	ctx context.Context,
	owner string,
	transcriptReference string,
	messagesBefore int,
	messagesAfter int,
) error {
	_, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.ContextAgentCompacted, Actor: "context", CorrelationID: owner,
		Data: event.ContextAgentCompactedData{
			Scope: owner, TranscriptReference: transcriptReference,
			MessagesBefore: messagesBefore, MessagesAfter: messagesAfter,
		},
	})
	return err
}
