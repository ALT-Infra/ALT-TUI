package provider

import (
	"context"
	"fmt"
	"strings"

	"altv1/internal/profile"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// capabilityAwareModel attempts rich input when support is unknown, then
// treats a narrowly recognized provider rejection as fresh capability
// evidence. The retry preserves ALT's manifest/path text and removes only the
// rejected image payload.
type capabilityAwareModel struct {
	base     model.BaseChatModel
	registry *Registry
	gateway  string
	spec     profile.Model
}

func (m *capabilityAwareModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	result, err := m.base.Generate(ctx, input, options...)
	if err == nil || !hasImageInput(input) || !isImageUnsupportedError(err) {
		return result, err
	}
	fallback, fallbackErr := m.base.Generate(ctx, stripImageInput(input), options...)
	if fallbackErr == nil || isExplicitImageUnsupportedError(err) {
		m.registry.markImageUnsupported(m.gateway, m.spec)
	}
	return fallback, fallbackErr
}

func (m *capabilityAwareModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	result, err := m.base.Stream(ctx, input, options...)
	if err == nil || !hasImageInput(input) || !isImageUnsupportedError(err) {
		return result, err
	}
	fallback, fallbackErr := m.base.Stream(ctx, stripImageInput(input), options...)
	if fallbackErr == nil || isExplicitImageUnsupportedError(err) {
		m.registry.markImageUnsupported(m.gateway, m.spec)
	}
	return fallback, fallbackErr
}

func (m *capabilityAwareModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	toolModel, ok := m.base.(model.ToolCallingChatModel)
	if !ok {
		return nil, fmt.Errorf("model %s/%s does not support tool binding", m.spec.Route, m.spec.Name)
	}
	bound, err := toolModel.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &capabilityAwareModel{
		base: bound, registry: m.registry, gateway: m.gateway, spec: m.spec,
	}, nil
}

func hasImageInput(messages []*schema.Message) bool {
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, part := range message.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL && part.Image != nil {
				return true
			}
		}
	}
	return false
}

func isImageUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	patterns := []string{
		"only supports text input",
		"unsupported content type 'image_url'",
		"unknown variant `image_url`, expected `text`",
		"image input is not supported",
		"does not support image input",
		"no endpoints found that support image input",
		"upstream request failed: [400] provider returned error",
	}
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func isExplicitImageUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"only supports text input",
		"unsupported content type 'image_url'",
		"unknown variant `image_url`, expected `text`",
		"image input is not supported",
		"does not support image input",
		"no endpoints found that support image input",
	} {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func stripImageInput(messages []*schema.Message) []*schema.Message {
	result := make([]*schema.Message, len(messages))
	for index, message := range messages {
		if message == nil {
			continue
		}
		copy := *message
		if len(message.UserInputMultiContent) == 0 {
			result[index] = &copy
			continue
		}
		parts := make([]schema.MessageInputPart, 0, len(message.UserInputMultiContent))
		textOnly := true
		var text strings.Builder
		for _, part := range message.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL {
				continue
			}
			parts = append(parts, part)
			if part.Type == schema.ChatMessagePartTypeText {
				text.WriteString(part.Text)
			} else {
				textOnly = false
			}
		}
		if textOnly {
			copy.Content = text.String()
			copy.UserInputMultiContent = nil
		} else {
			copy.UserInputMultiContent = parts
		}
		result[index] = &copy
	}
	return result
}
