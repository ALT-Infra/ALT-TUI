package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type FailureKind string

const (
	FailureContextOverflow FailureKind = "context_overflow"
	FailureRequestTooLarge FailureKind = "request_too_large"
	FailureRateLimited     FailureKind = "rate_limited"
	FailureTransient       FailureKind = "transient"
	FailurePermanent       FailureKind = "permanent"
)

type Failure struct {
	Kind         FailureKind
	ContextLimit int
	RetryAfter   time.Duration
	Err          error
}

func (e *Failure) Error() string {
	if e == nil || e.Err == nil {
		return "model request failed"
	}
	return e.Err.Error()
}

func (e *Failure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func FailureDetails(err error) (Failure, bool) {
	var failure *Failure
	if errors.As(err, &failure) && failure != nil {
		return *failure, true
	}
	return Failure{}, false
}

func NormalizeFailure(err error) error {
	return normalizeFailureWithHint(err, 0, 0)
}

func normalizeFailureWithHint(err error, observedStatus int, retryAfter time.Duration) error {
	if err == nil {
		return nil
	}
	if _, ok := FailureDetails(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	status := 0
	code := ""
	message := err.Error()
	var apiError *einoopenai.APIError
	if errors.As(err, &apiError) && apiError != nil {
		status = apiError.HTTPStatusCode
		code = strings.ToLower(strings.TrimSpace(fmt.Sprint(apiError.Code)))
		message = strings.TrimSpace(apiError.Message)
	}
	if status == 0 {
		status = observedStatus
	}
	combined := strings.ToLower(strings.Join([]string{code, message, err.Error()}, " "))
	if contextOverflowPattern.MatchString(combined) {
		return &Failure{Kind: FailureContextOverflow, ContextLimit: contextLimitFromMessage(combined), Err: err}
	}
	if status == httpStatusRequestEntityTooLarge || strings.Contains(combined, "request entity too large") || strings.Contains(combined, "payload too large") {
		return &Failure{Kind: FailureRequestTooLarge, Err: err}
	}
	if status == 429 || strings.Contains(code, "rate_limit") || strings.Contains(combined, "too many requests") {
		return &Failure{Kind: FailureRateLimited, RetryAfter: retryAfter, Err: err}
	}
	switch status {
	case 408, 425, 500, 502, 503, 504:
		return &Failure{Kind: FailureTransient, RetryAfter: retryAfter, Err: err}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return &Failure{Kind: FailureTransient, Err: err}
	}
	return &Failure{Kind: FailurePermanent, Err: err}
}

const httpStatusRequestEntityTooLarge = 413

var contextOverflowPattern = regexp.MustCompile(strings.Join([]string{
	`prompt is too long`,
	`input is too long for (?:the )?requested model`,
	`exceeds (?:the )?context window`,
	`input token count[^\n]*exceeds (?:the )?maximum`,
	`maximum prompt length is [0-9]+`,
	`reduce the length of (?:the )?messages`,
	`maximum context length is [0-9]+ tokens`,
	`exceeds the (?:available )?context (?:size|length)`,
	`greater than the context length`,
	`context window exceeds limit`,
	`exceeded model token limit`,
	`context[_ ]length[_ ]exceeded`,
	`context length is only [0-9]+ tokens`,
	`input length[^\n]*exceeds[^\n]*context length`,
	`prompt too long; exceeded (?:max )?context length`,
	`too large for model with [0-9]+ maximum context length`,
	`model_context_window_exceeded`,
}, `|`))

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`maximum context length is ([0-9]+) tokens`),
	regexp.MustCompile(`context length is only ([0-9]+) tokens`),
	regexp.MustCompile(`model with ([0-9]+) maximum context length`),
	regexp.MustCompile(`maximum prompt length is ([0-9]+)`),
}

func contextLimitFromMessage(message string) int {
	for _, pattern := range contextLimitPatterns {
		match := pattern.FindStringSubmatch(message)
		if len(match) != 2 {
			continue
		}
		value, err := strconv.Atoi(match[1])
		if err == nil && value > 0 {
			return value
		}
	}
	return 0
}

type failureNormalizingModel struct {
	base model.BaseChatModel
}

func normalizeModelFailures(base model.BaseChatModel) model.BaseChatModel {
	if base == nil {
		return nil
	}
	return &failureNormalizingModel{base: base}
}

func (m *failureNormalizingModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	result, err := m.base.Generate(ctx, input, options...)
	return result, NormalizeFailure(err)
}

func (m *failureNormalizingModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	result, err := m.base.Stream(ctx, input, options...)
	return result, NormalizeFailure(err)
}

func (m *failureNormalizingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	toolModel, ok := m.base.(model.ToolCallingChatModel)
	if !ok {
		return nil, fmt.Errorf("model does not support tool binding")
	}
	bound, err := toolModel.WithTools(tools)
	if err != nil {
		return nil, NormalizeFailure(err)
	}
	return &failureNormalizingModel{base: bound}, nil
}
