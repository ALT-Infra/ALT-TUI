package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
	"altv1/internal/tooling"
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
		// Without a gateway tokenizer or usage response, one encoded byte per
		// token is the only model-independent upper bound. The prior four-byte
		// heuristic understated code, identifiers, and multilingual text.
		EstimatedTokens: len(prompt),
	})
	if err != nil {
		return store.ContextEpoch{}, err
	}
	compacted := workingViewWasCompacted(prompt)
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

func workingViewWasCompacted(prompt string) bool {
	var value any
	if json.Unmarshal([]byte(prompt), &value) != nil {
		return false
	}
	var inspect func(any) bool
	inspect = func(current any) bool {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				if inspect(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range typed {
				switch key {
				case "compacted":
					if compacted, _ := item.(bool); compacted {
						return true
					}
				case "archived_evidence", "archived":
					if item != nil {
						return true
					}
				case "archived_rounds":
					if count, _ := item.(float64); count > 0 {
						return true
					}
				}
				if inspect(item) {
					return true
				}
			}
		}
		return false
	}
	return inspect(value)
}

func (r *sessionRuntime) recordAgentCompaction(
	ctx context.Context,
	record tooling.AgentCompactionRecord,
) error {
	_, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.ContextAgentCompacted, Actor: "context", CorrelationID: record.Scope,
		Data: event.ContextAgentCompactedData{
			Scope: record.Scope, Trigger: record.Trigger,
			TranscriptReference: record.TranscriptReference,
			MessagesBefore:      record.MessagesBefore,
			MessagesAfter:       record.MessagesAfter,
			EstimatedTokens:     record.EstimatedTokens,
			PromptCapacity:      record.PromptCapacity,
			HighWater:           record.HighWater,
		},
	})
	if err != nil || record.Summary == nil {
		return err
	}
	modelReference, spec, ok := r.compactionModel(record.Scope)
	if !ok {
		return nil
	}
	return recordUsage(
		ctx, r.store, r.session.ID, modelReference,
		"context-compaction:"+record.Scope, spec, record.Summary, record.Scope,
	)
}

func (r *sessionRuntime) compactionModel(owner string) (string, profile.Model, bool) {
	parts := strings.Split(owner, ":")
	if len(parts) < 2 {
		return "", profile.Model{}, false
	}
	var reference string
	switch parts[0] {
	case "agent", "peer":
		assignment, ok := r.profile.Agent(parts[1])
		if !ok {
			return "", profile.Model{}, false
		}
		reference = assignment.Model
	case "specialist":
		assignment, ok := r.profile.Specialist(parts[1])
		if !ok {
			return "", profile.Model{}, false
		}
		reference = assignment.Model
	default:
		return "", profile.Model{}, false
	}
	spec, ok := r.profile.Models[reference]
	return reference, spec, ok
}
