package tooling

import (
	"context"
	"encoding/json"
	"sync"

	"altv1/internal/provider"

	"github.com/cloudwego/eino/schema"
)

// contextBudget converts externally owned model ceilings plus observed usage
// into a per-call working envelope. It deliberately has no universal fallback
// context size or chars-per-token constant.
type contextBudget struct {
	limits provider.ModelLimits

	mu                 sync.Mutex
	observedUpperBound int
	lastToolDigest     string
	latestPlan         contextBudgetPlan
}

type contextBudgetPlan struct {
	Known          bool
	Estimated      int
	PromptCapacity int
	OutputReserve  int
	GrowthReserve  int
	HighWater      int
	LowWater       int
}

func newContextBudget(limits provider.ModelLimits) *contextBudget {
	return &contextBudget{limits: limits}
}

func (b *contextBudget) plan(messages []*schema.Message, tools []*schema.ToolInfo) contextBudgetPlan {
	estimated := b.count(messages, tools)
	outputReserve := observedOutputReserve(messages, b.limits.MaxOutput.Tokens)
	capacity := b.hardPromptCapacity(outputReserve)
	if capacity <= 0 {
		plan := contextBudgetPlan{Estimated: estimated, OutputReserve: outputReserve}
		b.remember(plan)
		return plan
	}
	growth := observedGrowthReserve(messages)
	// Reserve one observed model-visible increment before the hard ceiling.
	// This is trajectory-derived hysteresis: exact eviction/compaction happens
	// while the next similarly sized turn still fits, not after capacity has
	// already been crossed.
	high := capacity - growth
	if high < 0 {
		high = 0
	}
	low := high - growth
	if low < 0 {
		low = 0
	}
	plan := contextBudgetPlan{
		Known: true, Estimated: estimated, PromptCapacity: capacity,
		OutputReserve: outputReserve, GrowthReserve: growth,
		HighWater: high, LowWater: low,
	}
	b.remember(plan)
	return plan
}

func (b *contextBudget) remember(plan contextBudgetPlan) {
	b.mu.Lock()
	b.latestPlan = plan
	b.mu.Unlock()
}

// availableResultBytes reports the conservative model-visible room left by
// the latest actual request plan. One byte per token is the safe pre-tokenizer
// conversion; zero with known=true means the current trajectory must compact
// before an exact recall page can safely make progress.
func (b *contextBudget) availableResultBytes() (bytes int, known bool) {
	b.mu.Lock()
	plan := b.latestPlan
	b.mu.Unlock()
	if !plan.Known {
		return 0, false
	}
	return max(0, plan.HighWater-plan.Estimated), true
}

func (b *contextBudget) hardPromptCapacity(outputReserve int) int {
	contextTokens := b.limits.ContextWindow.Tokens
	inputTokens := b.limits.MaxInput.Tokens
	b.mu.Lock()
	observed := b.observedUpperBound
	b.mu.Unlock()
	if observed > 0 && (contextTokens <= 0 || observed < contextTokens) {
		contextTokens = observed
	}
	capacity := inputTokens
	if contextTokens > 0 {
		combinedCapacity := contextTokens - outputReserve
		if combinedCapacity < 0 {
			combinedCapacity = 0
		}
		if capacity <= 0 || combinedCapacity < capacity {
			capacity = combinedCapacity
		}
	}
	return capacity
}

// observeOverflow turns a rejected prompt into new route-local evidence. A
// parsed provider ceiling wins when present; otherwise the rejected estimate
// is a strict upper bound and every recovery iteration must reduce it.
func (b *contextBudget) observeOverflow(messages []*schema.Message, tools []*schema.ToolInfo, providerLimit int) {
	bound := providerLimit
	if bound <= 0 {
		bound = b.count(messages, tools) - 1
	}
	if bound <= 0 {
		return
	}
	b.mu.Lock()
	if b.observedUpperBound == 0 || bound < b.observedUpperBound {
		b.observedUpperBound = bound
	}
	b.mu.Unlock()
}

func (b *contextBudget) count(messages []*schema.Message, tools []*schema.ToolInfo) int {
	numerator, denominator := observedBytesPerToken(messages)
	latestUsage := -1
	base := 0
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.Assistant || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
			continue
		}
		usage := message.ResponseMeta.Usage
		if usage.PromptTokens <= 0 && usage.TotalTokens <= 0 {
			continue
		}
		base = usage.TotalTokens
		if usage.PromptTokens > 0 {
			base = usage.PromptTokens + usage.CompletionTokens
		}
		latestUsage = index
		break
	}
	start := 0
	if latestUsage >= 0 {
		start = latestUsage + 1
	}
	for _, message := range messages[start:] {
		base += estimateJSONTokens(messageForCounting(message), numerator, denominator)
	}

	toolDigest, toolBytes := toolHeaderForCounting(tools)
	b.mu.Lock()
	previousToolDigest := b.lastToolDigest
	b.lastToolDigest = toolDigest
	b.mu.Unlock()
	if latestUsage < 0 || toolDigest != previousToolDigest {
		base += estimateByteTokens(toolBytes, numerator, denominator)
	}
	return base
}

func observedBytesPerToken(messages []*schema.Message) (numerator, denominator int) {
	// A token represents at least one encoded byte. That byte-level upper bound
	// is used until actual gateway usage supplies a route/model calibration.
	numerator, denominator = 1, 1
	for _, message := range messages {
		if message == nil || message.Role != schema.Assistant || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
			continue
		}
		tokens := message.ResponseMeta.Usage.CompletionTokens
		if tokens <= 0 {
			continue
		}
		visible, err := json.Marshal(messageForCounting(message))
		if err != nil || len(visible) == 0 {
			continue
		}
		// Keep the smallest observed bytes/token ratio. This is conservative
		// across prose, code, tool JSON, and hidden reasoning tokens.
		if len(visible)*denominator < numerator*tokens {
			numerator, denominator = len(visible), tokens
		}
	}
	return numerator, denominator
}

func estimateJSONTokens(value any, numerator, denominator int) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return estimateByteTokens(len(encoded), numerator, denominator)
}

func estimateByteTokens(bytes, numerator, denominator int) int {
	if bytes <= 0 {
		return 0
	}
	if numerator <= 0 || denominator <= 0 {
		return bytes
	}
	return (bytes*denominator + numerator - 1) / numerator
}

func messageForCounting(message *schema.Message) *schema.Message {
	if message == nil {
		return nil
	}
	copy := *message
	copy.ResponseMeta = nil
	copy.Extra = nil
	return &copy
}

func toolHeaderForCounting(tools []*schema.ToolInfo) (string, int) {
	if len(tools) == 0 {
		return "", 0
	}
	clean := make([]schema.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		copy := *tool
		copy.Extra = nil
		clean = append(clean, copy)
	}
	encoded, _ := json.Marshal(clean)
	return string(encoded), len(encoded)
}

func observedOutputReserve(messages []*schema.Message, maximum int) int {
	reserve := 0
	for _, message := range messages {
		if message == nil || message.Role != schema.Assistant {
			continue
		}
		if message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
			reserve = max(reserve, message.ResponseMeta.Usage.CompletionTokens)
		}
	}
	if maximum > 0 && reserve > maximum {
		return maximum
	}
	return reserve
}

// observedGrowthReserve is the latest model-visible increment: a user input,
// or an assistant response plus its tool results. It provides low-water
// hysteresis from the active trajectory without allowing one ancient, already
// archived outlier to reserve the whole context forever.
func observedGrowthReserve(messages []*schema.Message) int {
	numerator, denominator := observedBytesPerToken(messages)
	end := len(messages)
	for end > 0 && (messages[end-1] == nil || messages[end-1].Role == schema.System) {
		end--
	}
	if end == 0 {
		return 0
	}
	start := end - 1
	if messages[start] != nil && messages[start].Role == schema.Tool {
		for start > 0 && messages[start-1] != nil && messages[start-1].Role == schema.Tool {
			start--
		}
		if start > 0 && messages[start-1] != nil && messages[start-1].Role == schema.Assistant {
			start--
		}
	}
	reserve := 0
	for _, item := range messages[start:end] {
		reserve += estimateJSONTokens(messageForCounting(item), numerator, denominator)
	}
	return reserve
}

func (b *contextBudget) estimateFresh(
	messages []*schema.Message,
	tools []*schema.ToolInfo,
	calibration []*schema.Message,
) int {
	numerator, denominator := observedBytesPerToken(calibration)
	total := 0
	for _, message := range messages {
		total += estimateJSONTokens(messageForCounting(message), numerator, denominator)
	}
	_, toolBytes := toolHeaderForCounting(tools)
	total += estimateByteTokens(toolBytes, numerator, denominator)
	return total
}

func (b *contextBudget) summarizationTokenCounter(_ context.Context, inputMessages []*schema.Message, tools []*schema.ToolInfo) (int64, error) {
	return int64(b.count(inputMessages, tools)), nil
}
