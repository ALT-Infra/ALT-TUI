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

type leadTurnAgent struct {
	runtime *sessionRuntime
	signals []Signal
}

func (a *leadTurnAgent) Name(context.Context) string {
	return "alt-lead"
}

func (a *leadTurnAgent) Description(context.Context) string {
	return "ALT's selected Lead for one adaptive planning turn"
}

func (a *leadTurnAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		defer func() {
			if recovered := recover(); recovered != nil {
				generator.Send(&adk.AgentEvent{Err: fmt.Errorf("lead agent panic: %v\n%s", recovered, debug.Stack())})
			}
		}()
		decision, err := a.runtime.decideLead(ctx, a.signals)
		if err != nil {
			generator.Send(&adk.AgentEvent{Err: err})
			return
		}
		generator.Send(&adk.AgentEvent{
			Output: &adk.AgentOutput{CustomizedOutput: decision},
		})
	}()
	return iterator
}

func (a *leadTurnAgent) Resume(ctx context.Context, _ *adk.ResumeInfo, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
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
