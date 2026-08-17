package tooling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"altv1/internal/provider"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	agentCompactionInstruction = `Condense the working exchange into a clear continuation brief. Preserve the current objective, user constraints, decisions already made, exact identifiers and paths, evidence that affects later choices, unresolved work, failed attempts, and the next concrete step. Distinguish observations from inferences. Do not invent missing details. Older evidence remains available through the exact transcript reference that ALT will attach, so make this brief useful for deciding what to reopen rather than trying to reproduce every byte. Return only the continuation brief as text and do not call tools.`
)

func (r *Runtime) contextCompactionHandler(
	ctx context.Context,
	owner string,
	summarizer model.BaseChatModel,
	budgets ...*contextBudget,
) (adk.ChatModelAgentMiddleware, error) {
	if summarizer == nil {
		return nil, nil
	}
	budget := newContextBudget(provider.ModelLimits{})
	if len(budgets) > 0 && budgets[0] != nil {
		budget = budgets[0]
	}
	return &cacheAlignedCompaction{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		runtime:                      r, owner: owner, model: summarizer, budget: budget,
	}, nil
}

type cacheAlignedCompaction struct {
	*adk.BaseChatModelAgentMiddleware
	runtime *Runtime
	owner   string
	model   model.BaseChatModel
	budget  *contextBudget
}

func (m *cacheAlignedCompaction) WrapModel(
	_ context.Context,
	base model.BaseChatModel,
	modelContext *adk.ModelContext,
) (model.BaseChatModel, error) {
	if base == nil {
		return nil, fmt.Errorf("context recovery model is required")
	}
	var tools []*schema.ToolInfo
	if modelContext != nil {
		tools = append([]*schema.ToolInfo(nil), modelContext.Tools...)
	}
	return &overflowRecoveringModel{base: base, compactor: m, tools: tools}, nil
}

type overflowRecoveringModel struct {
	base      model.BaseChatModel
	compactor *cacheAlignedCompaction
	tools     []*schema.ToolInfo
}

func (m *overflowRecoveringModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	current := input
	for {
		response, err := m.base.Generate(ctx, current, options...)
		failure, recognized := provider.FailureDetails(err)
		if err == nil || !recognized || failure.Kind != provider.FailureContextOverflow {
			return response, err
		}
		next, recoverErr := m.compactor.recoverOverflow(ctx, current, m.tools, failure, "provider_overflow", true)
		if recoverErr != nil {
			return nil, recoverErr
		}
		if !m.compactor.strictlySmaller(next, current, m.tools) {
			return nil, fmt.Errorf("context overflow recovery made no model-visible progress: %w", err)
		}
		current = next
	}
}

func (m *overflowRecoveringModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	current := input
	for {
		response, err := m.base.Stream(ctx, current, options...)
		failure, recognized := provider.FailureDetails(err)
		if err == nil || !recognized || failure.Kind != provider.FailureContextOverflow {
			return response, err
		}
		next, recoverErr := m.compactor.recoverOverflow(ctx, current, m.tools, failure, "provider_overflow", true)
		if recoverErr != nil {
			return nil, recoverErr
		}
		if !m.compactor.strictlySmaller(next, current, m.tools) {
			return nil, fmt.Errorf("context overflow recovery made no model-visible progress: %w", err)
		}
		current = next
	}
}

func (m *cacheAlignedCompaction) recoverOverflow(
	ctx context.Context,
	original []*schema.Message,
	tools []*schema.ToolInfo,
	failure provider.Failure,
	trigger string,
	learnFailure bool,
) ([]*schema.Message, error) {
	if learnFailure {
		m.budget.observeOverflow(original, tools, failure.ContextLimit)
	}
	reference, err := m.runtime.archiveAgentTranscript(ctx, m.owner, original)
	if err != nil {
		return nil, err
	}

	brief := "The previous model request exceeded the provider's current context ceiling. " +
		"Continue from the recent verbatim messages below and reopen exact older evidence when it becomes relevant."
	var summaryMessage *schema.Message
	previousHeadSize := m.budget.estimateFresh(original, tools, original)
	for {
		plan := m.budget.plan(original, tools)
		instructionReserve := m.budget.estimateFresh(
			[]*schema.Message{schema.UserMessage(agentCompactionInstruction)}, nil, original,
		)
		summaryCeiling := max(0, plan.PromptCapacity-instructionReserve)
		head := selectEmergencySummaryHead(m.budget, original, tools, min(plan.HighWater, summaryCeiling))
		if len(head) == 0 {
			break
		}
		headSize := m.budget.estimateFresh(head, tools, original)
		if headSize >= previousHeadSize {
			break
		}
		previousHeadSize = headSize
		summaryInput := append([]*schema.Message(nil), head...)
		summaryInput = append(summaryInput, schema.UserMessage(agentCompactionInstruction))
		summary, summaryErr := generateSummaryWithProviderSignals(ctx, m.model, summaryInput, model.WithTools(tools))
		if summaryErr == nil {
			if value := strings.TrimSpace(compactionMessageText(summary)); value != "" {
				brief = value
				summaryMessage = summary
			}
			break
		}
		summaryFailure, ok := provider.FailureDetails(summaryErr)
		if !ok || summaryFailure.Kind != provider.FailureContextOverflow {
			return nil, fmt.Errorf("context overflow summary failed: %w", summaryErr)
		}
		m.budget.observeOverflow(summaryInput, tools, summaryFailure.ContextLimit)
	}

	note := "\n\nExact pre-compaction transcript: " + reference +
		". Use context_open if a detail omitted from this working brief becomes relevant."
	briefMessage := schema.UserMessage("[ALT continuation brief]\n" + brief + note)
	plan := m.budget.plan(original, tools)
	final := m.finalizeWithVerbatimTail(original, briefMessage, tools, plan.LowWater)
	if m.runtime.options.RecordAgentCompaction != nil {
		if err := m.runtime.options.RecordAgentCompaction(ctx, AgentCompactionRecord{
			Scope: m.owner, Trigger: trigger, TranscriptReference: reference,
			MessagesBefore: len(original), MessagesAfter: len(final),
			EstimatedTokens: plan.Estimated, PromptCapacity: plan.PromptCapacity,
			HighWater: plan.HighWater, Summary: summaryMessage,
		}); err != nil {
			return nil, err
		}
	}
	// Exact recall and process paging consult the latest accepted working
	// trajectory, not the oversized pre-compaction request.
	m.budget.plan(final, tools)
	return final, nil
}

func (m *cacheAlignedCompaction) strictlySmaller(next, previous []*schema.Message, tools []*schema.ToolInfo) bool {
	return m.budget.estimateFresh(next, tools, previous) < m.budget.estimateFresh(previous, tools, previous)
}

func selectEmergencySummaryHead(
	budget *contextBudget,
	original []*schema.Message,
	tools []*schema.ToolInfo,
	highWater int,
) []*schema.Message {
	if budget == nil || highWater <= 0 {
		return nil
	}
	systemEnd := 0
	for systemEnd < len(original) && original[systemEnd] != nil && original[systemEnd].Role == schema.System {
		systemEnd++
	}
	best := 0
	for boundary := systemEnd + 1; boundary < len(original); boundary++ {
		if !protocolValidPrefix(original[:boundary]) || !protocolValidSuffix(original[boundary:]) {
			continue
		}
		candidate := make([]*schema.Message, 0, boundary+1)
		for _, message := range original[:boundary] {
			candidate = append(candidate, cloneModelVisibleMessage(message))
		}
		candidate = append(candidate, schema.UserMessage(agentCompactionInstruction))
		if budget.estimateFresh(candidate, tools, original) <= highWater {
			best = boundary
		}
	}
	if best <= systemEnd {
		return nil
	}
	result := make([]*schema.Message, 0, best)
	for _, message := range original[:best] {
		result = append(result, cloneModelVisibleMessage(message))
	}
	return result
}

func protocolValidPrefix(messages []*schema.Message) bool {
	pending := make(map[string]bool)
	for _, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role {
		case schema.Assistant:
			for _, call := range message.ToolCalls {
				if call.ID != "" {
					pending[call.ID] = true
				}
			}
		case schema.Tool:
			delete(pending, message.ToolCallID)
		default:
			if len(pending) > 0 {
				return false
			}
		}
	}
	return len(pending) == 0
}

type providerDirectedRetry struct {
	lastDelay time.Duration
}

// next accepts only a provider-authored availability signal that makes strict
// progress. A repeated or longer Retry-After stops instead of translating an
// outage into an arbitrary client retry count.
func (r *providerDirectedRetry) next(ctx context.Context, err error) (time.Duration, bool) {
	failure, ok := provider.FailureDetails(err)
	if !ok || (failure.Kind != provider.FailureTransient && failure.Kind != provider.FailureRateLimited) || failure.RetryAfter <= 0 {
		return 0, false
	}
	delay := failure.RetryAfter
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Now().Add(delay).After(deadline) {
		return 0, false
	}
	if r.lastDelay > 0 && delay >= r.lastDelay {
		return 0, false
	}
	r.lastDelay = delay
	return delay, true
}

// providerDirectedSummaryModel gives Eino's summarization middleware the same
// provider-directed retry semantics as emergency overflow recovery. Keeping
// the progress loop here avoids translating a provider's availability signal
// into an invented maximum retry count.
type providerDirectedSummaryModel struct {
	base model.BaseChatModel
}

func (m *providerDirectedSummaryModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	return generateSummaryWithProviderSignals(ctx, m.base, input, options...)
}

func (m *providerDirectedSummaryModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return m.base.Stream(ctx, input, options...)
}

func generateSummaryWithProviderSignals(
	ctx context.Context,
	chat model.BaseChatModel,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	retry := &providerDirectedRetry{}
	for {
		response, err := chat.Generate(ctx, input, options...)
		if err == nil {
			return response, nil
		}
		delay, ok := retry.next(ctx, err)
		if !ok {
			return response, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *cacheAlignedCompaction) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	modelContext *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil {
		return ctx, state, nil
	}
	plan := m.budget.plan(state.Messages, state.ToolInfos)
	if !plan.Known {
		return ctx, state, nil
	}
	instructionReserve := m.budget.estimateFresh(
		[]*schema.Message{schema.UserMessage(agentCompactionInstruction)}, nil, state.Messages,
	)
	summaryCeiling := max(0, plan.PromptCapacity-instructionReserve)
	trigger := min(plan.HighWater, summaryCeiling)
	if plan.Estimated <= trigger {
		return ctx, state, nil
	}
	tools := append([]*schema.ToolInfo(nil), state.ToolInfos...)
	sort.SliceStable(tools, func(i, j int) bool {
		if tools[i] == nil {
			return tools[j] != nil
		}
		if tools[j] == nil {
			return false
		}
		return tools[i].Name < tools[j].Name
	})
	if plan.Estimated > summaryCeiling {
		next, err := m.recoverOverflow(
			ctx, state.Messages, tools,
			provider.Failure{Kind: provider.FailureContextOverflow, ContextLimit: plan.PromptCapacity},
			"discovered_capacity", false,
		)
		if err != nil {
			return ctx, nil, err
		}
		copyState := *state
		copyState.Messages = next
		return ctx, &copyState, nil
	}
	middleware, err := summarization.New(ctx, &summarization.Config{
		Model: &providerDirectedSummaryModel{base: m.model},
		ModelOptions: []model.Option{
			model.WithTools(tools),
		},
		Trigger: &summarization.TriggerCondition{
			ContextTokens: trigger,
		},
		TokenCounter: func(_ context.Context, input *summarization.TokenCounterInput) (int, error) {
			return m.budget.count(input.Messages, input.Tools), nil
		},
		UserInstruction: agentCompactionInstruction,
		// The default summarizer prepends a different system message, which
		// invalidates the entire warm prefix. Replaying the exact request and
		// appending only the instruction lets the gateway reuse every eligible
		// token. The tools option above preserves the other cache-bearing header.
		GenModelInput: func(
			_ context.Context,
			_ *schema.Message,
			userInstruction *schema.Message,
			original []*schema.Message,
		) ([]*schema.Message, error) {
			messages := append([]*schema.Message(nil), original...)
			messages = append(messages, userInstruction)
			return messages, nil
		},
		Finalize: func(ctx context.Context, original []*schema.Message, summary *schema.Message) ([]*schema.Message, error) {
			reference, err := m.runtime.archiveAgentTranscript(ctx, m.owner, original)
			if err != nil {
				return nil, err
			}
			brief := strings.TrimSpace(compactionMessageText(summary))
			if brief == "" {
				return nil, fmt.Errorf("compaction summary is empty")
			}
			note := "\n\nExact pre-compaction transcript: " + reference +
				". Use context_open if a detail omitted from this working brief becomes relevant."
			briefMessage := schema.UserMessage("[ALT continuation brief]\n" + brief + note)
			final := m.finalizeWithVerbatimTail(original, briefMessage, tools, plan.LowWater)
			if m.runtime.options.RecordAgentCompaction != nil {
				if err := m.runtime.options.RecordAgentCompaction(ctx, AgentCompactionRecord{
					Scope: m.owner, Trigger: "working_budget", TranscriptReference: reference,
					MessagesBefore: len(original), MessagesAfter: len(final),
					EstimatedTokens: plan.Estimated, PromptCapacity: plan.PromptCapacity,
					HighWater: trigger, Summary: summary,
				}); err != nil {
					return nil, err
				}
			}
			m.budget.plan(final, tools)
			return final, nil
		},
	})
	if err != nil {
		return ctx, nil, err
	}
	return middleware.BeforeModelRewriteState(ctx, state, modelContext)
}

func compactionMessageText(message *schema.Message) string {
	if message == nil {
		return ""
	}
	var result strings.Builder
	result.WriteString(message.Content)
	for _, part := range message.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func (m *cacheAlignedCompaction) finalizeWithVerbatimTail(
	original []*schema.Message,
	brief *schema.Message,
	tools []*schema.ToolInfo,
	lowWater int,
) []*schema.Message {
	systemEnd := 0
	for systemEnd < len(original) && original[systemEnd] != nil && original[systemEnd].Role == schema.System {
		systemEnd++
	}
	base := make([]*schema.Message, 0, systemEnd+1)
	for _, message := range original[:systemEnd] {
		base = append(base, cloneModelVisibleMessage(message))
	}
	base = append(base, brief)
	if lowWater <= 0 || systemEnd >= len(original) {
		return base
	}

	best := len(original)
	for start := len(original) - 1; start >= systemEnd; start-- {
		if original[start] == nil || original[start].Role == schema.Tool || !protocolValidSuffix(original[start:]) {
			continue
		}
		candidate := append([]*schema.Message(nil), base...)
		for _, message := range original[start:] {
			candidate = append(candidate, cloneModelVisibleMessage(message))
		}
		if m.budget.estimateFresh(candidate, tools, original) <= lowWater {
			best = start
		}
	}
	if best == len(original) {
		return base
	}
	for _, message := range original[best:] {
		base = append(base, cloneModelVisibleMessage(message))
	}
	return base
}

func cloneModelVisibleMessage(message *schema.Message) *schema.Message {
	if message == nil {
		return nil
	}
	copy := *message
	copy.ResponseMeta = nil
	copy.Extra = nil
	return &copy
}

func protocolValidSuffix(messages []*schema.Message) bool {
	knownCalls := make(map[string]bool)
	for _, message := range messages {
		if message == nil {
			continue
		}
		if message.Role == schema.Assistant {
			for _, call := range message.ToolCalls {
				if call.ID != "" {
					knownCalls[call.ID] = true
				}
			}
			continue
		}
		if message.Role == schema.Tool && message.ToolCallID != "" && !knownCalls[message.ToolCallID] {
			return false
		}
	}
	return true
}

func (r *Runtime) archiveAgentTranscript(ctx context.Context, owner string, messages []*schema.Message) (string, error) {
	archivable := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			archivable = append(archivable, nil)
			continue
		}
		copy := *message
		if len(copy.UserInputMultiContent) > 0 {
			copy.UserInputMultiContent = append([]schema.MessageInputPart(nil), copy.UserInputMultiContent...)
			for index := range copy.UserInputMultiContent {
				part := &copy.UserInputMultiContent[index]
				if part.Image == nil || part.Extra == nil || part.Extra["alt_artifact_reference"] == nil {
					continue
				}
				imageCopy := *part.Image
				imageCopy.Base64Data = nil
				part.Image = &imageCopy
			}
		}
		if !r.options.PersistReasoning {
			copy.ReasoningContent = ""
			if len(copy.AssistantGenMultiContent) > 0 {
				parts := make([]schema.MessageOutputPart, 0, len(copy.AssistantGenMultiContent))
				for _, part := range copy.AssistantGenMultiContent {
					if part.Type == schema.ChatMessagePartTypeReasoning {
						continue
					}
					parts = append(parts, part)
				}
				copy.AssistantGenMultiContent = parts
			}
		}
		archivable = append(archivable, &copy)
	}
	content, err := json.Marshal(archivable)
	if err != nil {
		return "", fmt.Errorf("encode exact agent transcript: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(owner), 0), content...))
	reference := toolOutputPathPrefix + hex.EncodeToString(digest[:])
	path := filepath.Join(r.archive, "tool-output", hex.EncodeToString(digest[:]))
	if err := writeImmutableFile(path, content); err != nil {
		return "", err
	}
	if r.options.ArchiveToolOutput != nil {
		if err := r.options.ArchiveToolOutput(ctx, owner, reference, content); err != nil {
			return "", fmt.Errorf("index exact agent transcript: %w", err)
		}
	}
	return reference, nil
}

func writeImmutableFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create exact transcript directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(content); writeErr != nil {
			file.Close()
			return fmt.Errorf("write exact transcript: %w", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close exact transcript: %w", closeErr)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("create exact transcript: %w", err)
	}
	existing, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("verify exact transcript: %w", readErr)
	}
	if !bytes.Equal(existing, content) {
		return fmt.Errorf("exact transcript reference collision")
	}
	return nil
}
