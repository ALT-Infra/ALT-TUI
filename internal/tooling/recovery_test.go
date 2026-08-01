package tooling

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func TestRecoverableToolErrorsReturnsOrdinaryFailureToModel(t *testing.T) {
	middleware := RecoverableToolErrors()
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			return "", errors.New("path is a directory")
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("ordinary tool failure escaped as a fatal error: %v", err)
	}
	message, failed := ParseRecoverableToolError(result)
	if !failed || message != "path is a directory" {
		t.Fatalf("result = %q, parsed = (%q, %t)", result, message, failed)
	}
}

func TestRecoverableToolErrorsPreservesCancellation(t *testing.T) {
	middleware := RecoverableToolErrors()
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			return "", context.Canceled
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint(context.Background(), `{}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v, want context.Canceled", err)
	}
}

func TestRecoverableToolErrorsConvertsMidStreamFailure(t *testing.T) {
	middleware := RecoverableToolErrors()
	endpoint, err := middleware.WrapStreamableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
			reader, writer := schema.Pipe[string](1)
			go func() {
				defer writer.Close()
				writer.Send("partial output\n", nil)
				writer.Send("", errors.New("command exited 2"))
			}()
			return reader, nil
		},
		&adk.ToolContext{Name: "execute", CallID: "call-2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := endpoint(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ordinary stream failure escaped as fatal: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 2 || chunks[0] != "partial output\n" {
		t.Fatalf("chunks = %#v", chunks)
	}
	message, failed := ParseRecoverableToolError(chunks[1])
	if !failed || message != "command exited 2" {
		t.Fatalf("error chunk = %q, parsed = (%q, %t)", chunks[1], message, failed)
	}
}
