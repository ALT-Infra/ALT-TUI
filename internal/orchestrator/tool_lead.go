package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/tooling"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func (r *sessionRuntime) generateLeadDecisionWithTools(
	ctx context.Context,
	lead profile.LeadAssignment,
	turn int,
	system string,
	user string,
	allowUnstructuredFinalization bool,
	validate func(LeadDecision) error,
) (LeadDecision, error) {
	messages := []*schema.Message{schema.UserMessage(user)}
	strategies := []string{"initial", "explicit-correction"}
	var invalid error
	var invalidCandidate string
	for index, strategy := range strategies {
		chat, modelSpec, err := r.providers.Model(ctx, r.profile, lead.Model, provider.Text)
		if err != nil {
			return LeadDecision{}, err
		}
		chat = r.observeModel(lead.Model, "lead:"+lead.ID)(chat)
		var handlers []adk.ChatModelAgentMiddleware
		instruction := system + `
If you do not call a runtime tool, your final assistant message must be only
the required JSON decision. If you call any runtime tool, finish that tool-work
phase with a concise factual completion report instead; ALT will pass that
report to a fresh, tool-free structured transition call. Do not mix a
user-facing answer with a coordination decision.`
		capabilities := r.providers.Capabilities(r.profile.Models[lead.Model])
		if capabilities.ToolCalling == provider.CapabilityUnsupported {
			instruction += "\nThe authenticated gateway catalog explicitly marks this model as tool-call unsupported. Coordinate using the supplied durable state and delegated members; do not claim to inspect the workspace directly."
		} else {
			runtimeHandlers, err := r.tools.Handlers(ctx, "lead:"+lead.ID, nil)
			if err != nil {
				return LeadDecision{}, err
			}
			handlers = tooling.AgentHandlers(runtimeHandlers...)
			instruction += "\nUse inherited runtime tools only when this user task actually depends on workspace contents. Do not explore the workspace for self-contained conceptual or computational requests."
		}
		agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Name: "lead-" + lead.ID, Description: r.profile.LeadDefinition(lead),
			Instruction: instruction, Model: chat,
			Handlers:      handlers,
			MaxIterations: unboundedAgentIterations(),
		})
		if err != nil {
			return LeadDecision{}, fmt.Errorf("create Eino Lead agent: %w", err)
		}
		runner := adk.NewRunner(ctx, adk.RunnerConfig{
			Agent: agent, EnableStreaming: true, CheckPointStore: r.store,
		})
		iterator := runner.Run(
			ctx,
			messages,
			adk.WithCheckPointID(fmt.Sprintf("lead-tools:%s:%d:%s", r.session.ID, turn, strategy)),
		)
		var candidate string
		var runErr error
		usedTools := false
		for {
			item, ok := iterator.Next()
			if !ok {
				break
			}
			if item.Err != nil {
				runErr = item.Err
				break
			}
			if item.Output == nil || item.Output.MessageOutput == nil {
				continue
			}
			variant := item.Output.MessageOutput
			message, err := r.consumeAgentMessage(ctx, variant, "", lead.ID, false)
			if err != nil {
				runErr = err
				break
			}
			if message == nil {
				continue
			}
			switch variant.Role {
			case schema.Assistant:
				if len(message.ToolCalls) > 0 {
					usedTools = true
				}
				for _, call := range message.ToolCalls {
					if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
						Kind: event.ToolCalled, Actor: lead.ID,
						Data: event.ToolCallData{
							ToolCallID: call.ID, Tool: call.Function.Name,
							Arguments: call.Function.Arguments,
						},
					}); err != nil {
						return LeadDecision{}, err
					}
				}
				if text := strings.TrimSpace(messageText(message)); text != "" {
					candidate = text
				}
				if err := recordUsage(
					ctx, r.store, r.session.ID, lead.Model,
					"lead:"+lead.ID, modelSpec, message.ResponseMeta,
				); err != nil {
					return LeadDecision{}, err
				}
			case schema.Tool:
				usedTools = true
				toolError, failed := tooling.ParseRecoverableToolError(messageText(message))
				if _, err := r.store.Append(ctx, r.session.ID, event.Draft{
					Kind: event.ToolCompleted, Actor: lead.ID,
					Data: event.ToolCompletedData{
						ToolCallID: message.ToolCallID,
						Tool:       firstNonEmpty(message.ToolName, variant.ToolName),
						Failed:     failed,
						Error:      toolError,
						Result:     messageText(message),
					},
				}); err != nil {
					return LeadDecision{}, err
				}
			}
		}
		if runErr != nil {
			return LeadDecision{}, runErr
		}
		if usedTools {
			transitionSystem := system + `

This is the tool-free coordination transition after the Lead's tool-work phase.
Use the supplied completion report as evidence. Do not redo the work, do not
answer the user, and return only the required JSON decision.`
			transitionUser := user + "\n\nLEAD TOOL-WORK COMPLETION REPORT:\n" + candidate
			decision, transitionErr := generateStructured[LeadDecision](
				ctx, r.session.ID, r.store, r.providers, r.profile,
				lead.Model, "lead:"+lead.ID+":transition",
				transitionSystem, transitionUser, validate,
				r.observeModel(lead.Model, "lead:"+lead.ID+":transition"),
			)
			if transitionErr == nil {
				return decision, nil
			}
			invalid = transitionErr
			invalidCandidate = candidate
			break
		}
		decision, err := decodeJSONObject[LeadDecision](candidate)
		if err != nil && index > 0 {
			decision, err = decodeEnvelopedJSONObject[LeadDecision](candidate)
		}
		if err == nil && validate != nil {
			err = validate(decision)
		}
		if err == nil {
			return decision, nil
		}
		invalid = err
		invalidCandidate = candidate
		if index+1 < len(strategies) {
			messages = append(messages, schema.UserMessage(
				"Your decision cannot advance the session: "+
					invalid.Error()+
					". Correct that exact defect. You must either create useful work, wait for work that is actually active, or finalize with a complete brief. Return only the complete required JSON object.",
			))
		}
	}
	if allowUnstructuredFinalization {
		brief := strings.TrimSpace(invalidCandidate)
		if brief != "" {
			return LeadDecision{
				Assessment: "Recovered an unstructured Lead completion after all active work ended.",
				Finalize:   true,
				FinalBrief: brief,
			}, nil
		}
	}
	return LeadDecision{}, fmt.Errorf("Lead decision remained invalid after explicit correction: %w", invalid)
}
