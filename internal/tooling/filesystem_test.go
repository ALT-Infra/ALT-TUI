package tooling

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	localbackend "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk"
	adkfilesystem "github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"
)

func TestRootedBackendRejectsLexicalAndSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	backend := &rootedBackend{root: root}
	if _, err := backend.resolve("../outside/secret"); err == nil {
		t.Fatal("lexical escape was accepted")
	}
	if _, err := backend.resolve("escape/secret"); err == nil {
		t.Fatal("symlink escape was accepted")
	}
	got, err := backend.resolve("safe/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "safe", "new.txt")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
}

func TestRootedBackendConfinesVirtualToolOutputToPrivateRuntime(t *testing.T) {
	root := t.TempDir()
	temp := t.TempDir()
	backend := &rootedBackend{root: root, temp: temp}
	got, err := backend.resolve(toolOutputPathPrefix + "result-id")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(temp, "tool-output", "result-id")
	if got != want {
		t.Fatalf("virtual output resolved to %q, want %q", got, want)
	}
	if _, err := backend.resolve(toolOutputPathPrefix + "../../escape"); err == nil {
		t.Fatal("virtual tool-output traversal was accepted")
	}
}

func TestLargeToolResultIsPreservedOutsideModelContext(t *testing.T) {
	workspace := t.TempDir()
	runtime, err := NewRuntime(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	local, err := localbackend.NewBackend(context.Background(), &localbackend.Config{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &rootedBackend{root: workspace, temp: runtime.temp, delegate: local}
	reducer, err := runtime.toolResultReductionHandler(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	full := strings.Repeat("path/to/a/file\n", 10_000)
	wrapped, err := reducer.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) { return full, nil },
		&adk.ToolContext{Name: "glob", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := wrapped(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) >= len(full) || !strings.Contains(visible, "<persisted-output>") {
		t.Fatalf("model-visible result was not reduced: %d bytes", len(visible))
	}
	virtualPath := runtime.toolOutputPath("glob", "call-1")
	if !strings.Contains(visible, virtualPath) {
		t.Fatalf("reduced result does not expose its read_file path: %q", visible)
	}
	firstPage, err := backend.Read(context.Background(), &adkfilesystem.ReadRequest{
		FilePath: virtualPath,
		Offset:   1,
		Limit:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(firstPage.Content, "\n") != 99 {
		t.Fatalf("read_file page contains %d line breaks, want 99", strings.Count(firstPage.Content, "\n"))
	}
	storedPath, err := backend.resolve(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != full {
		t.Fatalf("offloaded result changed: got %d bytes, want %d", len(stored), len(full))
	}
}

func TestRuntimeHandlersExposeExactCapabilitySet(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handlers, err := runtime.Handlers(
		context.Background(),
		"member:test",
		[]string{"read_file", "ls"},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext := &adk.ChatModelAgentContext{}
	for _, handler := range handlers {
		_, runContext, err = handler.BeforeAgent(context.Background(), runContext)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runContext.Tools) != 2 {
		t.Fatalf("tool count = %d, want 2", len(runContext.Tools))
	}
	names := make(map[string]bool, len(runContext.Tools))
	for _, runtimeTool := range runContext.Tools {
		info, err := runtimeTool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	if !names["read_file"] || !names["ls"] || names["write_file"] {
		t.Fatalf("exposed tools = %#v", names)
	}
}

func TestWebResearchIsAvailableToAnyAssignmentThatReceivesIt(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handlers, err := runtime.Handlers(
		context.Background(),
		"member:any-role",
		[]string{ToolNameWebSearch},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext := &adk.ChatModelAgentContext{}
	for _, handler := range handlers {
		_, runContext, err = handler.BeforeAgent(context.Background(), runContext)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runContext.Tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(runContext.Tools))
	}
	info, err := runContext.Tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != ToolNameWebSearch {
		t.Fatalf("tool = %q, want %q", info.Name, ToolNameWebSearch)
	}
}
