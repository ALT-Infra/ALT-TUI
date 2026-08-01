package tooling

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

const recoverableToolErrorPrefix = "ALT_TOOL_ERROR: "

// AgentHandlers returns the runtime-tool middleware in execution order.
// Eino makes the first handler the outermost wrapper, so recovery must precede
// the middleware which installs the filesystem tools.
func AgentHandlers(middlewares ...adk.ChatModelAgentMiddleware) []adk.ChatModelAgentMiddleware {
	handlers := []adk.ChatModelAgentMiddleware{RecoverableToolErrors()}
	for _, middleware := range middlewares {
		if middleware != nil {
			handlers = append(handlers, middleware)
		}
	}
	return handlers
}

// RecoverableToolErrors turns an ordinary tool failure into a tool result the
// model can inspect and correct. Runtime cancellation and deadlines remain
// fatal because continuing after either would violate session control.
func RecoverableToolErrors() adk.ChatModelAgentMiddleware {
	return &recoverableToolErrors{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
	}
}

// ParseRecoverableToolError identifies the explicit marker stored in a tool
// result. Callers use it to record a failed tool node in execution provenance.
func ParseRecoverableToolError(result string) (string, bool) {
	index := strings.Index(result, recoverableToolErrorPrefix)
	if index < 0 {
		return "", false
	}
	return strings.TrimSpace(result[index+len(recoverableToolErrorPrefix):]), true
}

type recoverableToolErrors struct {
	*adk.BaseChatModelAgentMiddleware
}

func (m *recoverableToolErrors) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	_ *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
		result, err := endpoint(ctx, arguments, opts...)
		if err == nil {
			return result, nil
		}
		if fatalToolError(ctx, err) {
			return "", err
		}
		return recoverableToolErrorPrefix + err.Error(), nil
	}, nil
}

func (m *recoverableToolErrors) WrapStreamableToolCall(
	_ context.Context,
	endpoint adk.StreamableToolCallEndpoint,
	_ *adk.ToolContext,
) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, arguments string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		stream, err := endpoint(ctx, arguments, opts...)
		if err != nil {
			if fatalToolError(ctx, err) {
				return nil, err
			}
			return schema.StreamReaderFromArray([]string{recoverableToolErrorPrefix + err.Error()}), nil
		}
		return recoverStringStream(ctx, stream), nil
	}, nil
}

func (m *recoverableToolErrors) WrapEnhancedInvokableToolCall(
	_ context.Context,
	endpoint adk.EnhancedInvokableToolCallEndpoint,
	_ *adk.ToolContext,
) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, argument *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		result, err := endpoint(ctx, argument, opts...)
		if err == nil {
			return result, nil
		}
		if fatalToolError(ctx, err) {
			return nil, err
		}
		return toolErrorResult(err), nil
	}, nil
}

func (m *recoverableToolErrors) WrapEnhancedStreamableToolCall(
	_ context.Context,
	endpoint adk.EnhancedStreamableToolCallEndpoint,
	_ *adk.ToolContext,
) (adk.EnhancedStreamableToolCallEndpoint, error) {
	return func(ctx context.Context, argument *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
		stream, err := endpoint(ctx, argument, opts...)
		if err != nil {
			if fatalToolError(ctx, err) {
				return nil, err
			}
			return schema.StreamReaderFromArray([]*schema.ToolResult{toolErrorResult(err)}), nil
		}
		return recoverEnhancedStream(ctx, stream), nil
	}, nil
}

func fatalToolError(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func toolErrorResult(err error) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{
		Type: schema.ToolPartTypeText,
		Text: recoverableToolErrorPrefix + err.Error(),
	}}}
}

func recoverStringStream(
	ctx context.Context,
	source *schema.StreamReader[string],
) *schema.StreamReader[string] {
	reader, writer := schema.Pipe[string](1)
	go func() {
		defer writer.Close()
		defer source.Close()
		for {
			chunk, err := source.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				if fatalToolError(ctx, err) {
					writer.Send("", err)
				} else {
					writer.Send(recoverableToolErrorPrefix+err.Error(), nil)
				}
				return
			}
			if writer.Send(chunk, nil) {
				return
			}
		}
	}()
	return reader
}

func recoverEnhancedStream(
	ctx context.Context,
	source *schema.StreamReader[*schema.ToolResult],
) *schema.StreamReader[*schema.ToolResult] {
	reader, writer := schema.Pipe[*schema.ToolResult](1)
	go func() {
		defer writer.Close()
		defer source.Close()
		for {
			chunk, err := source.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				if fatalToolError(ctx, err) {
					writer.Send(nil, err)
				} else {
					writer.Send(toolErrorResult(err), nil)
				}
				return
			}
			if writer.Send(chunk, nil) {
				return
			}
		}
	}()
	return reader
}
