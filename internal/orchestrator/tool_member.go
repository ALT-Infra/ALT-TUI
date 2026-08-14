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

func (r *sessionRuntime) runToolMember(
	ctx context.Context,
	member profile.MemberAssignment,
	delegation *Delegation,
	attempt int,
) (string, error) {
	capabilities := r.providers.Capabilities(r.profile.Gateway, r.profile.Models[member.Model])
	if capabilities.ToolCalling == provider.CapabilityUnsupported {
		return "", fmt.Errorf(
			"authenticated gateway catalog marks member %s model %s as tool-call unsupported; ALT assignments require dynamic runtime tools",
			member.ID,
			r.profile.Models[member.Model].Name,
		)
	}
	chat, modelSpec, err := r.providers.Model(ctx, r.profile, member.Model, provider.Text)
	if err != nil {
		return "", err
	}
	chat = r.observeModel(
		member.Model,
		"member:"+member.ID,
		delegation.Spec.ID,
	)(chat)
	toolOwner := fmt.Sprintf("member:%s:%s:%d", member.ID, delegation.Spec.ID, attempt)
	runtimeHandlers, err := r.tools.HandlersWithCompaction(ctx, toolOwner, chat)
	if err != nil {
		return "", err
	}
	system, user := memberMessages(r.profile, member, delegation)
	userMessage, err := r.richUserMessage(ctx, member.Model, user, delegation.Spec.Attachments)
	if err != nil {
		return "", err
	}
	if _, err := r.commitWorkingView(ctx, "specialist", delegation.Spec.ID, delegation.SpecSequence, user); err != nil {
		return "", err
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "member-" + member.ID,
		Description:   r.profile.MemberDefinition(member),
		Instruction:   system,
		Model:         chat,
		Handlers:      tooling.AgentHandlers(runtimeHandlers...),
		MaxIterations: unboundedAgentIterations(),
	})
	if err != nil {
		return "", fmt.Errorf("create Eino member agent: %w", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent, EnableStreaming: true, CheckPointStore: r.store,
	})
	iterator := runner.Run(
		ctx,
		[]*schema.Message{userMessage},
		adk.WithCheckPointID(fmt.Sprintf("delegation:%s:%d", delegation.Spec.ID, attempt)),
	)

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
		message, err := r.consumeAgentMessage(
			ctx,
			variant,
			delegation.Spec.ID,
			member.ID,
			true,
		)
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
					Kind: event.ToolCalled, Actor: member.ID,
					CorrelationID: delegation.Spec.ID,
					Data: event.ToolCallData{
						DelegationID: delegation.Spec.ID,
						ToolCallID:   call.ID,
						Tool:         call.Function.Name,
						Provider:     r.tools.ProviderForTool(ctx, call.Function.Name),
						Arguments:    call.Function.Arguments,
					},
				}); err != nil {
					return "", err
				}
			}
			if text := strings.TrimSpace(messageText(message)); text != "" {
				finalCandidate = text
			}
			if err := recordUsage(
				ctx,
				r.store,
				r.session.ID,
				member.Model,
				"member:"+member.ID,
				modelSpec,
				message.ResponseMeta,
				delegation.Spec.ID,
			); err != nil {
				return "", err
			}
		case schema.Tool:
			toolError, failed := tooling.ParseRecoverableToolError(messageText(message))
			if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
				Kind: event.ToolCompleted, Actor: member.ID,
				CorrelationID: delegation.Spec.ID,
				Data: event.ToolCompletedData{
					DelegationID: delegation.Spec.ID,
					ToolCallID:   message.ToolCallID,
					Tool:         firstNonEmpty(message.ToolName, variant.ToolName),
					Failed:       failed,
					Error:        toolError,
					Result:       messageText(message),
				},
			}); err != nil {
				return "", err
			}
		}
	}
	return finalCandidate, nil
}

func (r *sessionRuntime) consumeAgentMessage(
	ctx context.Context,
	variant *adk.MessageVariant,
	delegationID string,
	actor string,
	persistDeltas bool,
) (*schema.Message, error) {
	if !variant.IsStreaming {
		return variant.Message, nil
	}
	if variant.MessageStream == nil {
		return nil, nil
	}
	defer variant.MessageStream.Close()
	var chunks []*schema.Message
	textBuffer := newEventTextBuffer(256, func(text string) error {
		_, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.DelegationTextDelta, Actor: actor,
			CorrelationID: delegationID,
			Data:          event.TextDeltaData{DelegationID: delegationID, Text: text},
		})
		return err
	})
	reasoningBuffer := newEventTextBuffer(1024, func(text string) error {
		_, err := r.store.Append(ctx, r.session.ID, event.Draft{
			Kind: event.DelegationReasoning, Actor: actor,
			CorrelationID: delegationID,
			Data:          event.TextDeltaData{DelegationID: delegationID, Text: text},
		})
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
		if variant.Role != schema.Assistant || !persistDeltas {
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
		return nil, fmt.Errorf("combine Eino agent stream: %w", err)
	}
	return message, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
