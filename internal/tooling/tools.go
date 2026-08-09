package tooling

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const (
	ToolNameExecCommand = "exec_command"
	ToolNameWriteStdin  = "write_stdin"
	ToolNameApplyPatch  = "apply_patch"
	ToolNameWebSearch   = "web_search"
	ToolNameWebFetch    = "web_fetch"
	ToolNameWebAnswer   = "web_answer"
)

var allRuntimeTools = append(
	append([]string(nil), filesystemTools...),
	ToolNameExecCommand,
	ToolNameWriteStdin,
	ToolNameApplyPatch,
	ToolNameWebSearch,
	ToolNameWebFetch,
	ToolNameWebAnswer,
)

func ToolNames() []string {
	return append([]string(nil), allRuntimeTools...)
}

// Handlers gives every assignment the complete runtime tool catalogue through
// Eino's dynamic ToolSearch middleware. Process sessions remain scoped to
// owner, so concurrent assignments cannot write to or poll each other's PTYs.
func (r *Runtime) Handlers(
	ctx context.Context,
	owner string,
) ([]adk.ChatModelAgentMiddleware, error) {
	if r == nil {
		return nil, fmt.Errorf("tool runtime is required")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("tool owner is required")
	}
	enabled := make(map[string]bool, len(allRuntimeTools))
	for _, name := range allRuntimeTools {
		enabled[name] = true
	}

	filesystemTools, filesystemInstruction, reducer, err := r.filesystemTools(ctx)
	if err != nil {
		return nil, err
	}
	nativeTools, err := r.nativeTools(ctx, owner, enabled)
	if err != nil {
		return nil, err
	}
	deferred := append([]tool.BaseTool(nil), filesystemTools...)
	deferred = append(deferred, nativeTools...)
	dynamic, err := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: deferred})
	if err != nil {
		return nil, fmt.Errorf("create Eino dynamic tool search: %w", err)
	}
	handlers := []adk.ChatModelAgentMiddleware{
		&toolMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
			instruction:                  filesystemInstruction,
		},
		dynamic,
		reducer,
	}
	if len(nativeTools) > 0 {
		executionPolicy := "exec_command runs inside ALT's fail-closed Linux sandbox: Bubblewrap isolates filesystem mounts, processes, IPC, UTS, and direct networking; no_new_privs and Landlock restrict privilege transitions and writes. It may read the host filesystem, but may write only the session workspace and ALT's private temporary directory."
		if r.options.DangerouslyBypassApprovalsAndSandbox {
			executionPolicy = "DANGEROUS MODE: exec_command bypasses ALT's approval and sandbox boundary and executes directly on the host. Treat every command as having the user's ambient operating-system authority."
		}
		handlers[0] = &toolMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
			instruction:                  filesystemInstruction + "\n" + executionPolicy + " Running commands return a numeric session_id for write_stdin; copy that exact ID when polling or sending input. An unknown-process lookup does not prove that the process ended: if the ID was transcribed incorrectly, retry with the exact session_id from the latest running result. apply_patch accepts strict Git or standard unified text patches and validates every path against the workspace before changing files. Discover runtime tools with tool_search as the work requires them. web_search discovers sources, web_fetch retrieves exact source content, and web_answer supplies an independent cited synthesis. Search output and generated answers are not substitutes for checking decisive claims against fetched primary evidence. Credentials are never exposed to shell commands.",
		}
	}
	return handlers, nil
}

func (r *Runtime) nativeTools(ctx context.Context, owner string, enabled map[string]bool) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	if enabled[ToolNameExecCommand] {
		value, err := toolutils.InferTool(
			ToolNameExecCommand,
			"Execute a shell command in the session workspace. Returns output and an exit code, or a session_id when still running.",
			func(ctx context.Context, input ExecCommandInput) (ProcessResult, error) {
				return r.processes.start(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWriteStdin] {
		value, err := toolutils.InferTool(
			ToolNameWriteStdin,
			"Write exact characters to, or poll, a running exec_command session owned by this assignment.",
			func(ctx context.Context, input WriteStdinInput) (ProcessResult, error) {
				return r.processes.write(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameApplyPatch] {
		applier := &patchApplier{root: r.root}
		value, err := toolutils.InferTool(
			ToolNameApplyPatch,
			"Apply a strict Git or standard unified text patch transactionally inside the session workspace. The complete patch must contain file headers and hunks.",
			applier.apply,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	researchProvider, researchErr := r.researchProvider(ctx)
	if researchErr != nil && (enabled[ToolNameWebSearch] || enabled[ToolNameWebFetch] || enabled[ToolNameWebAnswer]) {
		return r.nativeToolsWithUnavailableResearch(owner, enabled, tools, researchErr)
	}
	if enabled[ToolNameWebSearch] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebSearch,
			"Discover web sources through Exa. Supports fast through deep-reasoning search, domain/date/category controls, alternate deep queries, current content extraction, subpage discovery, and grounded synthesized output. Use web_fetch to verify decisive sources.",
			r.exaWebSearch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebFetch] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebFetch,
			"Retrieve exact URLs or Exa document IDs with configurable text, highlights, structured summaries, freshness, semantic sections, subpages, and outgoing links. Use returned statuses and page content as evidence.",
			r.exaWebFetch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebAnswer] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebAnswer,
			"Ask Exa for an independent answer with citations, optionally structured by JSON Schema. Treat it as a cross-check and inspect or fetch the cited sources before relying on material claims.",
			r.exaWebAnswer,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if researchProvider == "linkup" {
		return r.appendLinkupTools(enabled, tools)
	}
	return tools, nil
}

type unavailableResearchInput struct {
	Query string   `json:"query,omitempty" jsonschema:"description=Research query, retained so the setup error can explain how to enable web research."`
	URLs  []string `json:"urls,omitempty" jsonschema:"description=Exact URLs intended for retrieval."`
}

func (r *Runtime) researchProvider(ctx context.Context) (string, error) {
	if r.options.ResolveResearchProvider == nil {
		if r.options.ResolveExaCredential != nil {
			return "exa", nil
		}
		return "", fmt.Errorf("web research is not configured; choose a provider with /research")
	}
	provider, err := r.options.ResolveResearchProvider(ctx)
	if err != nil {
		return "", err
	}
	switch provider {
	case "exa", "linkup":
		return provider, nil
	default:
		return "", fmt.Errorf("unsupported selected research provider %q", provider)
	}
}

// ProviderForTool pins the provider used by a provider-backed tool into the
// durable call event. It is deliberately empty for local tools and when setup
// is incomplete; execution still returns the authoritative setup error.
func (r *Runtime) ProviderForTool(ctx context.Context, name string) string {
	switch name {
	case ToolNameWebSearch, ToolNameWebFetch, ToolNameWebAnswer:
		provider, err := r.researchProvider(ctx)
		if err == nil {
			return provider
		}
	}
	return ""
}

func (r *Runtime) nativeToolsWithUnavailableResearch(
	owner string,
	enabled map[string]bool,
	tools []tool.BaseTool,
	researchErr error,
) ([]tool.BaseTool, error) {
	_ = owner
	for _, specification := range []struct {
		name        string
		description string
	}{
		{ToolNameWebSearch, "Web source discovery is unavailable until the user configures and selects a research provider."},
		{ToolNameWebFetch, "Exact web retrieval is unavailable until the user configures and selects a research provider."},
		{ToolNameWebAnswer, "Independent cited web synthesis is unavailable until the user configures and selects a research provider."},
	} {
		if !enabled[specification.name] {
			continue
		}
		value, err := toolutils.InferTool(
			specification.name,
			specification.description,
			func(context.Context, unavailableResearchInput) (WebResearchResult, error) {
				return WebResearchResult{}, researchErr
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	return tools, nil
}

type toolMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	instruction string
	tools       []tool.BaseTool
}

func (m *toolMiddleware) BeforeAgent(
	ctx context.Context,
	runCtx *adk.ChatModelAgentContext,
) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, nil, nil
	}
	copy := *runCtx
	if m.instruction != "" {
		copy.Instruction += "\n" + m.instruction
	}
	copy.Tools = append(copy.Tools, m.tools...)
	return ctx, &copy, nil
}
