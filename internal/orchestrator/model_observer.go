package orchestrator

import (
	"context"
	"errors"

	"altv1/internal/event"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var errToolCallingUnsupported = errors.New("gateway model does not support tool calling")

type observedChatModel struct {
	delegate model.BaseChatModel
	before   func(context.Context) error
}

func (m *observedChatModel) Generate(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	if err := m.before(ctx); err != nil {
		return nil, err
	}
	return m.delegate.Generate(ctx, input, options...)
}

func (m *observedChatModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	if err := m.before(ctx); err != nil {
		return nil, err
	}
	return m.delegate.Stream(ctx, input, options...)
}

func (m *observedChatModel) WithTools(
	tools []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	calling, ok := m.delegate.(model.ToolCallingChatModel)
	if !ok {
		return nil, errToolCallingUnsupported
	}
	bound, err := calling.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &observedChatModel{delegate: bound, before: m.before}, nil
}

func (r *sessionRuntime) observeModel(
	modelReference string,
	purpose string,
	correlationID ...string,
) func(model.BaseChatModel) model.BaseChatModel {
	return func(chat model.BaseChatModel) model.BaseChatModel {
		return &observedChatModel{
			delegate: chat,
			before: func(ctx context.Context) error {
				correlation := ""
				if len(correlationID) > 0 {
					correlation = correlationID[0]
				}
				_, err := r.store.Append(ctx, r.session.ID, event.Draft{
					Kind: event.ModelCallStarted, Actor: purpose,
					CorrelationID: correlation,
					Data: event.ModelCallStartedData{
						Model: modelReference, Purpose: purpose,
					},
				})
				return err
			},
		}
	}
}
