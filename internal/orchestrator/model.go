package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Eino treats zero as a fixed default of 20 model/tool cycles. ALT uses the
// platform's largest int as the library's effective "unbounded" sentinel so
// cancellation, gateway enforcement, and the model's own completion determine
// termination instead of an invented ALT ceiling.
func unboundedAgentIterations() int {
	return int(^uint(0) >> 1)
}

func generateStructured[T any](
	ctx context.Context,
	sessionID string,
	ledger *store.Store,
	registry *provider.Registry,
	p profile.Profile,
	modelReference string,
	purpose string,
	system string,
	user string,
	validate func(T) error,
	prepare func(model.BaseChatModel) model.BaseChatModel,
) (T, error) {
	var zero T
	chat, spec, err := registry.Model(ctx, p, modelReference, provider.Structured)
	if err != nil {
		return zero, err
	}
	if prepare != nil {
		chat = prepare(chat)
	}
	messages := []*schema.Message{schema.SystemMessage(system), schema.UserMessage(user)}
	response, err := chat.Generate(ctx, messages)
	if err != nil {
		return zero, err
	}
	if usageErr := recordUsage(ctx, ledger, sessionID, modelReference, purpose, spec, response.ResponseMeta); usageErr != nil {
		return zero, usageErr
	}
	responseText := messageText(response)
	value, parseErr := decodeJSONObject[T](responseText)
	if parseErr == nil && validate != nil {
		parseErr = validate(value)
	}
	if parseErr == nil {
		return value, nil
	}

	// A malformed structured answer is recoverable with one distinct strategy:
	// an explicit correction containing the observed validation failure. We do
	// not repeat transport errors or cycle through an arbitrary retry count.
	messages = append(messages,
		structuredCorrectionMessage(responseText),
		schema.UserMessage("Your response was invalid: "+parseErr.Error()+". Correct that exact defect and return only the required JSON object."),
	)
	corrected, err := chat.Generate(ctx, messages)
	if err != nil {
		return zero, fmt.Errorf("%s correction request failed: %w", purpose, err)
	}
	if usageErr := recordUsage(ctx, ledger, sessionID, modelReference, purpose+":correction", spec, corrected.ResponseMeta); usageErr != nil {
		return zero, usageErr
	}
	value, correctionErr := decodeJSONObject[T](messageText(corrected))
	if correctionErr == nil && validate != nil {
		correctionErr = validate(value)
	}
	if correctionErr != nil {
		return zero, fmt.Errorf("%s remained invalid after explicit correction: %w", purpose, correctionErr)
	}
	return value, nil
}

// structuredCorrectionMessage preserves the malformed text without replaying
// provider-only reasoning, unmatched tool calls, or an empty assistant frame.
// Some OpenAI-compatible gateways reject assistant messages that contain
// neither text nor tool calls, so an empty first attempt must still produce a
// valid correction conversation.
func structuredCorrectionMessage(raw string) *schema.Message {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "[The previous response contained no JSON content.]"
	}
	return schema.AssistantMessage(raw, nil)
}

func streamText(
	ctx context.Context,
	chat model.BaseChatModel,
	onChunk func(*schema.Message) error,
	messages ...*schema.Message,
) (*schema.ResponseMeta, string, error) {
	reader, err := chat.Stream(ctx, messages)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()

	var content strings.Builder
	var meta *schema.ResponseMeta
	for {
		chunk, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return meta, content.String(), err
		}
		if chunk == nil {
			continue
		}
		if err := onChunk(chunk); err != nil {
			return meta, content.String(), err
		}
		content.WriteString(messageText(chunk))
		if chunk.ResponseMeta != nil {
			meta = chunk.ResponseMeta
		}
	}
	return meta, content.String(), nil
}

func messageText(message *schema.Message) string {
	if message == nil {
		return ""
	}
	var text strings.Builder
	text.WriteString(message.Content)
	for _, part := range message.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func messageReasoning(message *schema.Message) string {
	if message == nil {
		return ""
	}
	var text strings.Builder
	text.WriteString(message.ReasoningContent)
	for _, part := range message.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil {
			text.WriteString(part.Reasoning.Text)
		}
	}
	return text.String()
}

func decodeJSONObject[T any](raw string) (T, error) {
	var zero T
	raw = strings.TrimSpace(raw)
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("decode JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return zero, fmt.Errorf("decode JSON: trailing content")
	}
	return value, nil
}

// decodeEnvelopedJSONObject is a deterministic recovery for models that put
// prose or a Markdown fence around an otherwise valid response. It accepts
// exactly one balanced top-level JSON object and still subjects that object to
// the strict decoder (including unknown-field rejection). Multiple objects are
// ambiguous and therefore remain invalid.
func decodeEnvelopedJSONObject[T any](raw string) (T, error) {
	if value, err := decodeJSONObject[T](raw); err == nil {
		return value, nil
	}

	var objects []string
	depth := 0
	start := -1
	inString := false
	escaped := false
	for index, current := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch current {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			continue
		}
		switch current {
		case '{':
			if depth == 0 {
				start = index
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				objects = append(objects, raw[start:index+1])
				start = -1
			}
		}
	}
	if depth != 0 || len(objects) != 1 {
		var zero T
		return zero, fmt.Errorf(
			"decode JSON envelope: found %d complete top-level objects with final depth %d",
			len(objects), depth,
		)
	}
	return decodeJSONObject[T](objects[0])
}

func recordUsage(
	ctx context.Context,
	ledger *store.Store,
	sessionID string,
	modelReference string,
	purpose string,
	spec profile.Model,
	meta *schema.ResponseMeta,
	correlationID ...string,
) error {
	if meta == nil || meta.Usage == nil {
		return nil
	}
	usage := meta.Usage
	correlation := ""
	if len(correlationID) > 0 {
		correlation = correlationID[0]
	}
	_, err := ledger.Append(ctx, sessionID, event.Draft{
		Kind:          event.ModelUsage,
		Actor:         purpose,
		CorrelationID: correlation,
		Data: event.ModelUsageData{
			Model:            modelReference + ":" + spec.Name,
			Purpose:          purpose,
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
			TotalTokens:      usage.TotalTokens,
		},
	})
	return err
}
