package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/tooling"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// generateAgentOutcome runs the current leadership-capable model once. Plain output
// is the user answer. A deliberately marked coordination object is an ALT
// control transition. This lets an agent inspect, use tools, and answer in one
// run without paying for a mandatory planning call and a second synthesis call.
func (r *sessionRuntime) generateAgentOutcome(
	ctx context.Context,
	agent profile.AgentAssignment,
	turn int,
	system string,
	exactUserMessage *schema.Message,
	snapshot string,
	observedWork int,
	surface *modelSurface,
) (AgentOutcome, error) {
	capabilities := r.providers.Capabilities(r.profile.Gateway, r.profile.Models[agent.Model])
	instruction := system
	if capabilities.ToolCalling == provider.CapabilityUnsupported {
		instruction += coordinationFallbackInstruction
	}

	messages, snapshotDigest, err := prepareModelSurfaceMessages(
		surface, instruction, r.session.ID, exactUserMessage, snapshot,
	)
	if err != nil {
		return AgentOutcome{}, err
	}
	strategies := []string{"initial", "explicit-correction"}
	var lastInvalid error
	for index, strategy := range strategies {
		modelContext := provider.WithCacheScope(ctx, r.session.ConversationID+":agent:"+agent.ID)
		chat, modelSpec, err := r.providers.Model(modelContext, r.profile, agent.Model, provider.Text)
		if err != nil {
			return AgentOutcome{}, err
		}
		chat = r.observeModel(agent.Model, "agent:"+agent.ID)(chat)
		var handlers []adk.ChatModelAgentMiddleware
		var toolsConfig adk.ToolsConfig
		owner := fmt.Sprintf("agent:%s:%d:%s", agent.ID, turn, strategy)
		if capabilities.ToolCalling != provider.CapabilityUnsupported {
			coordination, returnDirectly, err := coordinationTools()
			if err != nil {
				return AgentOutcome{}, err
			}
			toolsConfig = adk.ToolsConfig{
				ToolsNodeConfig: compose.ToolsNodeConfig{Tools: coordination},
				ReturnDirectly:  returnDirectly,
			}
			runtimeHandlers, err := r.tools.HandlersWithCompaction(
				modelContext, owner, chat,
				r.providers.Limits(r.profile.Gateway, modelSpec),
			)
			if err != nil {
				return AgentOutcome{}, err
			}
			handlers = tooling.AgentHandlers(runtimeHandlers...)
		} else {
			handlers, err = r.tools.CompactionHandlers(
				modelContext, owner, chat,
				r.providers.Limits(r.profile.Gateway, modelSpec),
			)
			if err != nil {
				return AgentOutcome{}, err
			}
		}
		handlers = append(handlers, newModelSurfaceHandler(
			r.store, r.session.ID,
			r.profile.Gateway+":"+agent.Model+":"+modelSpec.Name,
			surface, snapshotDigest,
		))

		runnerAgent, err := adk.NewChatModelAgent(modelContext, &adk.ChatModelAgentConfig{
			Name: "agent-" + agent.ID, Description: r.profile.AgentDefinition(agent),
			Instruction: instruction, Model: chat, Handlers: handlers,
			ToolsConfig:   toolsConfig,
			MaxIterations: unboundedAgentIterations(),
		})
		if err != nil {
			return AgentOutcome{}, fmt.Errorf("create active Eino agent: %w", err)
		}
		runner := adk.NewRunner(modelContext, adk.RunnerConfig{
			Agent: runnerAgent, EnableStreaming: true, CheckPointStore: r.store,
		})
		iterator := runner.Run(modelContext, messages, adk.WithCheckPointID(
			fmt.Sprintf("agent:%s:%s:%d:%s", r.session.ID, agent.ID, turn, strategy),
		))
		var candidate string
		coordinationToolReturned := false
		for {
			item, ok := iterator.Next()
			if !ok {
				break
			}
			if item.Err != nil {
				return AgentOutcome{}, item.Err
			}
			if item.Output == nil || item.Output.MessageOutput == nil {
				continue
			}
			variant := item.Output.MessageOutput
			message, err := r.consumeAgentMessage(ctx, variant, "", agent.ID, false)
			if err != nil {
				return AgentOutcome{}, err
			}
			if message == nil {
				continue
			}
			switch variant.Role {
			case schema.Assistant:
				for _, call := range message.ToolCalls {
					if isCoordinationTool(call.Function.Name) {
						continue
					}
					if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
						Kind: event.ToolCalled, Actor: agent.ID,
						Data: event.ToolCallData{
							ToolCallID: call.ID, Tool: call.Function.Name,
							Provider:  r.tools.ProviderForTool(ctx, call.Function.Name),
							Arguments: call.Function.Arguments,
						},
					}); err != nil {
						return AgentOutcome{}, err
					}
				}
				if text := strings.TrimSpace(messageText(message)); text != "" {
					candidate = text
				}
				if err := recordUsage(ctx, r.store, r.session.ID, agent.Model, "agent:"+agent.ID, modelSpec, message); err != nil {
					return AgentOutcome{}, err
				}
			case schema.Tool:
				toolName := firstNonEmpty(message.ToolName, variant.ToolName)
				if isCoordinationTool(toolName) {
					coordinationToolReturned = true
					if text := strings.TrimSpace(messageText(message)); text != "" {
						candidate = text
					}
					continue
				}
				toolError, failed := tooling.ParseRecoverableToolError(messageText(message))
				if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
					Kind: event.ToolCompleted, Actor: agent.ID,
					Data: event.ToolCompletedData{
						ToolCallID: message.ToolCallID,
						Tool:       toolName,
						Failed:     failed, Error: toolError, Result: messageText(message),
					},
				}); err != nil {
					return AgentOutcome{}, err
				}
			}
		}

		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			lastInvalid = fmt.Errorf("model returned no answer or coordination transition")
		} else if coordinationToolReturned || looksLikeCoordination(candidate) {
			decision, err := decodeJSONObject[AgentDecision](candidate)
			if err == nil {
				err = validateAgentDecision(decision, observedWork)
			}
			if err == nil {
				return AgentOutcome{Decision: &decision}, nil
			}
			lastInvalid = err
		} else {
			return AgentOutcome{Answer: candidate}, nil
		}

		if index+1 < len(strategies) {
			messages, _, err = prepareModelSurfaceMessages(
				surface, instruction, r.session.ID, exactUserMessage, snapshot,
			)
			if err != nil {
				return AgentOutcome{}, err
			}
			messages = append(messages, schema.UserMessage(
				"Your attempted ALT coordination transition was invalid: "+lastInvalid.Error()+
					". Return either the complete user-facing answer or one corrected kind=coordinate JSON object.",
			))
		}
	}
	return AgentOutcome{}, fmt.Errorf("coordination transition remained invalid after explicit correction: %w", lastInvalid)
}

func looksLikeCoordination(raw string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &object) != nil {
		return false
	}
	kind, ok := object["kind"]
	if !ok {
		return false
	}
	var value string
	return json.Unmarshal(kind, &value) == nil && value == "coordinate"
}

func validateAgentDecision(decision AgentDecision, observedWork int) error {
	if decision.Kind != "coordinate" {
		return fmt.Errorf("kind must be coordinate")
	}
	if strings.TrimSpace(decision.Assessment) == "" {
		return fmt.Errorf("assessment is empty")
	}
	if decision.Handoff != nil && (len(decision.Delegations) > 0 || len(decision.PeerTurns) > 0) {
		return fmt.Errorf("handoff must be exclusive")
	}
	if decision.Handoff != nil && (strings.TrimSpace(decision.Handoff.PeerID) == "" || strings.TrimSpace(decision.Handoff.Reason) == "") {
		return fmt.Errorf("handoff peer_id and reason are required")
	}
	if decision.Handoff == nil && len(decision.Delegations) == 0 && len(decision.PeerTurns) == 0 && observedWork == 0 {
		return fmt.Errorf("no work, handoff, or answer was produced")
	}
	return nil
}
