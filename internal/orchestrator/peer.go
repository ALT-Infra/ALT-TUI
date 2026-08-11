package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/tooling"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func (r *sessionRuntime) executePeerTurn(parent context.Context, turnID string, attempt int) {
	state, err := r.projection(parent)
	if err != nil {
		r.stopWithError(err)
		return
	}
	turn := state.PeerTurns[turnID]
	if turn == nil || turn.Status == DelegationCompleted || turn.Status == DelegationCancelled {
		return
	}
	lead, ok := r.profile.Lead(state.LeadID)
	if !ok {
		r.stopWithError(fmt.Errorf("selected Lead %s is absent", state.LeadID))
		return
	}
	peer, ok := r.profile.PeerMemberFor(lead, turn.Spec.PeerID)
	if !ok {
		r.stopWithError(fmt.Errorf("peer %s is not permitted", turn.Spec.PeerID))
		return
	}

	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.active[turnID] = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, turnID)
		r.mu.Unlock()
	}()

	if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
		Kind: event.PeerTurnStarted, Actor: peer.ID, CorrelationID: turnID,
		Data: event.PeerTurnStartedData{PeerTurnID: turnID, Attempt: attempt},
	}); err != nil {
		r.stopWithError(err)
		return
	}

	raw, runErr := r.runPeerMember(ctx, peer, turn, state.CollaborationTurns(turn.Spec.CollaborationID), state.LastSequence, attempt)
	if runErr != nil {
		r.handlePeerTurnFailure(parent, turnID, peer.ID, attempt, runErr)
		return
	}
	result, parseErr := decodeJSONObject[MemberResult](raw)
	if parseErr != nil {
		result = MemberResult{Result: strings.TrimSpace(raw)}
	}
	if strings.TrimSpace(result.Result) == "" {
		r.handlePeerTurnFailure(parent, turnID, peer.ID, attempt, fmt.Errorf("peer returned an empty result"))
		return
	}
	completed, err := r.store.Append(parent, r.session.ID, event.Draft{
		Kind: event.PeerTurnCompleted, Actor: peer.ID, CorrelationID: turnID,
		Data: event.PeerTurnCompletedData{
			PeerTurnID: turnID, Attempt: attempt, Result: result.Result,
			Findings: result.Findings, Risks: result.Risks, Confidence: result.Confidence,
		},
	})
	if err != nil {
		r.stopWithError(err)
		return
	}
	r.pushSignal(Signal{Kind: string(event.PeerTurnCompleted), EventID: completed.ID, DelegationID: turnID})
}

func (r *sessionRuntime) runPeerMember(
	ctx context.Context,
	peer profile.MemberAssignment,
	turn *PeerTurn,
	history []*PeerTurn,
	sourceThrough int64,
	attempt int,
) (string, error) {
	capabilities := r.providers.Capabilities(r.profile.Gateway, r.profile.Models[peer.Model])
	if capabilities.ToolCalling == provider.CapabilityUnsupported {
		return "", fmt.Errorf("authenticated gateway catalog marks peer %s model %s as tool-call unsupported; ALT assignments require dynamic runtime tools", peer.ID, r.profile.Models[peer.Model].Name)
	}
	chat, modelSpec, err := r.providers.Model(ctx, r.profile, peer.Model, provider.Text)
	if err != nil {
		return "", err
	}
	chat = r.observeModel(peer.Model, "peer:"+peer.ID, turn.Spec.ID)(chat)
	toolOwner := fmt.Sprintf("peer:%s:%s:%d:%d", peer.ID, turn.Spec.CollaborationID, turn.Spec.Round, attempt)
	handlers, err := r.tools.HandlersWithCompaction(ctx, toolOwner, chat)
	if err != nil {
		return "", err
	}
	system, user := peerMessages(r.profile, peer, turn, history)
	if _, err := r.commitWorkingView(ctx, "peer", turn.Spec.CollaborationID, sourceThrough, user); err != nil {
		return "", err
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "peer-" + peer.ID, Description: r.profile.MemberDefinition(peer),
		Instruction: system, Model: chat, Handlers: tooling.AgentHandlers(handlers...),
		MaxIterations: unboundedAgentIterations(),
	})
	if err != nil {
		return "", fmt.Errorf("create Eino peer agent: %w", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: true, CheckPointStore: r.store})
	iterator := runner.Run(ctx, []*schema.Message{schema.UserMessage(user)}, adk.WithCheckPointID(fmt.Sprintf("peer:%s:%s:%d:%d", r.session.ID, turn.Spec.CollaborationID, turn.Spec.Round, attempt)))
	var finalCandidate string
	for {
		item, ok := iterator.Next()
		if !ok {
			break
		}
		if item.Err != nil {
			return "", item.Err
		}
		if item.Output == nil || item.Output.MessageOutput == nil {
			continue
		}
		variant := item.Output.MessageOutput
		message, err := r.consumePeerAgentMessage(ctx, variant, turn.Spec.ID, peer.ID)
		if err != nil {
			return "", err
		}
		if message == nil {
			continue
		}
		switch variant.Role {
		case schema.Assistant:
			for _, call := range message.ToolCalls {
				if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
					Kind: event.ToolCalled, Actor: peer.ID, CorrelationID: turn.Spec.ID,
					Data: event.ToolCallData{DelegationID: turn.Spec.ID, ToolCallID: call.ID, Tool: call.Function.Name, Provider: r.tools.ProviderForTool(ctx, call.Function.Name), Arguments: call.Function.Arguments},
				}); err != nil {
					return "", err
				}
			}
			if text := strings.TrimSpace(messageText(message)); text != "" {
				finalCandidate = text
			}
			if err := recordUsage(ctx, r.store, r.session.ID, peer.Model, "peer:"+peer.ID, modelSpec, message.ResponseMeta, turn.Spec.ID); err != nil {
				return "", err
			}
		case schema.Tool:
			toolError, failed := tooling.ParseRecoverableToolError(messageText(message))
			if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
				Kind: event.ToolCompleted, Actor: peer.ID, CorrelationID: turn.Spec.ID,
				Data: event.ToolCompletedData{DelegationID: turn.Spec.ID, ToolCallID: message.ToolCallID, Tool: firstNonEmpty(message.ToolName, variant.ToolName), Failed: failed, Error: toolError, Result: messageText(message)},
			}); err != nil {
				return "", err
			}
		}
	}
	return finalCandidate, nil
}

func (r *sessionRuntime) consumePeerAgentMessage(ctx context.Context, variant *adk.MessageVariant, turnID, actor string) (*schema.Message, error) {
	if !variant.IsStreaming {
		return variant.Message, nil
	}
	if variant.MessageStream == nil {
		return nil, nil
	}
	defer variant.MessageStream.Close()
	var chunks []*schema.Message
	textBuffer := newEventTextBuffer(256, func(text string) error {
		_, err := r.store.Append(ctx, r.session.ID, event.Draft{Kind: event.PeerTextDelta, Actor: actor, CorrelationID: turnID, Data: event.PeerTextDeltaData{PeerTurnID: turnID, Text: text}})
		return err
	})
	reasoningBuffer := newEventTextBuffer(1024, func(text string) error {
		_, err := r.store.Append(ctx, r.session.ID, event.Draft{Kind: event.PeerReasoning, Actor: actor, CorrelationID: turnID, Data: event.PeerTextDeltaData{PeerTurnID: turnID, Text: text}})
		return err
	})
	for {
		chunk, err := variant.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			continue
		}
		chunks = append(chunks, chunk)
		if variant.Role != schema.Assistant {
			continue
		}
		if text := messageText(chunk); text != "" {
			if err := textBuffer.Add(text); err != nil {
				return nil, err
			}
		}
		if reasoning := messageReasoning(chunk); reasoning != "" && r.profile.Policy.PersistReasoning {
			if err := reasoningBuffer.Add(reasoning); err != nil {
				return nil, err
			}
		}
	}
	if err := textBuffer.Flush(); err != nil {
		return nil, err
	}
	if err := reasoningBuffer.Flush(); err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	message, err := schema.ConcatMessages(chunks)
	if err != nil {
		return nil, fmt.Errorf("combine Eino peer stream: %w", err)
	}
	return message, nil
}

func (r *sessionRuntime) handlePeerTurnFailure(ctx context.Context, turnID, actor string, attempt int, cause error) {
	state, err := r.projection(context.WithoutCancel(ctx))
	if err == nil {
		if turn := state.PeerTurns[turnID]; turn != nil && turn.Status == DelegationCancelled {
			return
		}
	}
	if errors.Is(cause, context.Canceled) && !r.run.userCancelled.Load() {
		return
	}
	failed, err := r.store.Append(context.WithoutCancel(ctx), r.session.ID, event.Draft{
		Kind: event.PeerTurnFailed, Actor: actor, CorrelationID: turnID,
		Data: event.PeerTurnFailedData{PeerTurnID: turnID, Attempt: attempt, Error: cause.Error()},
	})
	if err != nil {
		r.stopWithError(err)
		return
	}
	r.pushSignal(Signal{Kind: string(event.PeerTurnFailed), EventID: failed.ID, DelegationID: turnID})
}
