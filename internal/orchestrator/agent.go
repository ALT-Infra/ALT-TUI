package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type activeTurnAgent struct {
	runtime *sessionRuntime
	signals []Signal
}

func (a *activeTurnAgent) Name(context.Context) string {
	return "alt-active-agent"
}

func (a *activeTurnAgent) Description(context.Context) string {
	return "ALT's current leadership-capable agent for one adaptive turn"
}

func (a *activeTurnAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		defer func() {
			if recovered := recover(); recovered != nil {
				generator.Send(&adk.AgentEvent{Err: fmt.Errorf("active agent panic: %v\n%s", recovered, debug.Stack())})
			}
		}()
		outcome, err := a.runtime.runActiveAgent(ctx, a.signals)
		if err != nil {
			generator.Send(&adk.AgentEvent{Err: err})
			return
		}
		generator.Send(&adk.AgentEvent{
			Output: &adk.AgentOutput{CustomizedOutput: outcome},
		})
	}()
	return iterator
}

func (a *activeTurnAgent) Resume(ctx context.Context, _ *adk.ResumeInfo, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.Run(ctx, &adk.AgentInput{}, options...)
}

type streamingAgent struct {
	name        string
	description string
	model       model.BaseChatModel
	messages    []*schema.Message
}

func (a *streamingAgent) Name(context.Context) string {
	return a.name
}

func (a *streamingAgent) Description(context.Context) string {
	return a.description
}

func (a *streamingAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		defer func() {
			if recovered := recover(); recovered != nil {
				generator.Send(&adk.AgentEvent{Err: fmt.Errorf("%s panic: %v\n%s", a.name, recovered, debug.Stack())})
			}
		}()
		reader, err := a.model.Stream(ctx, a.messages)
		if err != nil {
			generator.Send(&adk.AgentEvent{Err: err})
			return
		}
		defer reader.Close()
		for {
			chunk, err := reader.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					generator.Send(&adk.AgentEvent{Err: err})
				}
				return
			}
			if chunk == nil {
				continue
			}
			generator.Send(adk.EventFromMessage(chunk, nil, schema.Assistant, ""))
		}
	}()
	return iterator
}
