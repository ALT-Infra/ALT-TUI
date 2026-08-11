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
	"github.com/cloudwego/eino/schema"
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
	backend := &rootedBackend{root: root, archive: temp}
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
	if err := backend.Write(context.Background(), &adkfilesystem.WriteRequest{
		FilePath: toolOutputPathPrefix + "result-id", Content: "overwrite",
	}); err == nil {
		t.Fatal("model-facing filesystem backend overwrote a private archive")
	}
	if err := backend.Edit(context.Background(), &adkfilesystem.EditRequest{
		FilePath: toolOutputPathPrefix + "result-id", OldString: "a", NewString: "b",
	}); err == nil {
		t.Fatal("model-facing filesystem backend edited a private archive")
	}
}

func TestReductionBackendCannotOverwriteAnExactArchiveReference(t *testing.T) {
	root := t.TempDir()
	archive := t.TempDir()
	backend := &rootedBackend{
		root: root, archive: archive, allowArchiveWrite: true,
	}
	reference := toolOutputPathPrefix + "immutable-result"
	if err := backend.Write(context.Background(), &adkfilesystem.WriteRequest{
		FilePath: reference, Content: "first exact bytes",
	}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Write(context.Background(), &adkfilesystem.WriteRequest{
		FilePath: reference, Content: "different bytes",
	}); err == nil {
		t.Fatal("reduction backend overwrote an exact archive reference")
	}
	path, err := backend.resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first exact bytes" {
		t.Fatalf("archive was changed to %q", content)
	}
	if err := backend.Write(context.Background(), &adkfilesystem.WriteRequest{
		FilePath: reference, Content: "first exact bytes",
	}); err != nil {
		t.Fatalf("idempotent exact write failed: %v", err)
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
	backend := &rootedBackend{
		root: workspace, archive: runtime.archive, delegate: local,
		allowArchiveWrite: true, archiveOwner: "test", archiveOutput: runtime.options.ArchiveToolOutput,
	}
	reducer, err := runtime.toolResultReductionHandler(context.Background(), "test", backend)
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
	virtualPath := runtime.toolOutputPathFor("test", "glob", "call-1")
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

func TestLargeToolResultArchiveSurvivesRuntimeClose(t *testing.T) {
	workspace := t.TempDir()
	archive := filepath.Join(t.TempDir(), "request-context")
	var indexedOwner, indexedReference string
	var indexedContent []byte
	runtime, err := NewRuntimeWithOptions(context.Background(), workspace, RuntimeOptions{
		ContextArchiveDirectory: archive,
		ArchiveToolOutput: func(_ context.Context, owner, reference string, content []byte) error {
			indexedOwner, indexedReference = owner, reference
			indexedContent = append([]byte(nil), content...)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	local, err := localbackend.NewBackend(context.Background(), &localbackend.Config{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &rootedBackend{
		root: workspace, archive: runtime.archive, delegate: local,
		allowArchiveWrite: true, archiveOwner: "test", archiveOutput: runtime.options.ArchiveToolOutput,
	}
	reducer, err := runtime.toolResultReductionHandler(context.Background(), "test", backend)
	if err != nil {
		t.Fatal(err)
	}
	full := strings.Repeat("durable evidence\n", 10_000)
	wrapped, err := reducer.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) { return full, nil },
		&adk.ToolContext{Name: "read_file", CallID: "durable-call"},
	)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := wrapped(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	virtualPath := runtime.toolOutputPathFor("test", "read_file", "durable-call")
	if !strings.Contains(visible, virtualPath) {
		t.Fatalf("reduced result omitted durable reference: %q", visible)
	}
	if indexedOwner != "test" || indexedReference != virtualPath || string(indexedContent) != full {
		t.Fatal("exact reduction output did not reach the durable artifact index")
	}
	storedPath, err := backend.resolve(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatalf("read archive after Runtime.Close: %v", err)
	}
	if string(stored) != full {
		t.Fatalf("durable archive changed: got %d bytes, want %d", len(stored), len(full))
	}
}

func TestHistoricalToolRoundIsClearedOnlyAfterExactDurableOffload(t *testing.T) {
	workspace := t.TempDir()
	archive := filepath.Join(t.TempDir(), "request-context")
	runtime, err := NewRuntimeWithOptions(context.Background(), workspace, RuntimeOptions{
		ContextArchiveDirectory: archive,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	local, err := localbackend.NewBackend(context.Background(), &localbackend.Config{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &rootedBackend{root: workspace, archive: runtime.archive, delegate: local, allowArchiveWrite: true}
	reducer, err := runtime.toolResultReductionHandler(context.Background(), "test", backend)
	if err != nil {
		t.Fatal(err)
	}
	oldResult := strings.Repeat("exact-old-observation\n", 16_000)
	messages := []adk.Message{
		schema.SystemMessage("system"), schema.UserMessage("work"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-old", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage(oldResult, "call-old"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-new-1", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage("recent one", "call-new-1"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-new-2", Type: "function", Function: schema.FunctionCall{Name: "grep", Arguments: `{}`}}}),
		schema.ToolMessage("recent two", "call-new-2"),
	}
	_, rewritten, err := reducer.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{Messages: messages}, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	visible := rewritten.Messages[3].Content
	virtualPath := runtime.toolOutputPathFor("test", "cleared:grep", "call-old")
	if !strings.Contains(visible, virtualPath) || strings.Contains(visible, "exact-old-observation") {
		t.Fatalf("old tool result was not replaced by exact archive reference: %q", visible)
	}
	storedPath, err := backend.resolve(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != oldResult {
		t.Fatalf("cleared tool result changed: got %d bytes, want %d", len(stored), len(oldResult))
	}
}

func TestRuntimeHandlersExposeCompleteCatalogueThroughToolSearch(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handlers, err := runtime.Handlers(context.Background(), "member:test")
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
	if len(runContext.Tools) != len(ToolNames())+1 {
		t.Fatalf("agent tool count = %d, want %d runtime tools plus tool_search", len(runContext.Tools), len(ToolNames()))
	}
	names := make(map[string]bool, len(runContext.Tools))
	for _, runtimeTool := range runContext.Tools {
		info, err := runtimeTool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	for _, name := range ToolNames() {
		if !names[name] {
			t.Fatalf("complete runtime catalogue is missing %q: %#v", name, names)
		}
	}
	if !names["tool_search"] {
		t.Fatalf("dynamic discovery tool is absent: %#v", names)
	}

	infos := make([]*schema.ToolInfo, 0, len(runContext.Tools))
	for _, runtimeTool := range runContext.Tools {
		info, infoErr := runtimeTool.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		infos = append(infos, info)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("inspect the workspace")}, ToolInfos: infos}
	_, state, err = handlers[1].BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ToolInfos) != 1 || state.ToolInfos[0].Name != "tool_search" {
		t.Fatalf("initial model-visible tools = %#v, want only tool_search", state.ToolInfos)
	}
}

func TestWebResearchIsDiscoverableByEveryAssignment(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handlers, err := runtime.Handlers(context.Background(), "member:any-role")
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
	found := false
	for _, runtimeTool := range runContext.Tools {
		info, infoErr := runtimeTool.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		found = found || info.Name == ToolNameWebSearch
	}
	if !found {
		t.Fatalf("%s is absent from the complete discoverable catalogue", ToolNameWebSearch)
	}
}
