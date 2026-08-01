package tooling

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const (
	ToolNameExecCommand = "exec_command"
	ToolNameWriteStdin  = "write_stdin"
	ToolNameApplyPatch  = "apply_patch"
	ToolNameWebSearch   = "web_search"
)

var allRuntimeTools = append(
	append([]string(nil), filesystemTools...),
	ToolNameExecCommand,
	ToolNameWriteStdin,
	ToolNameApplyPatch,
	ToolNameWebSearch,
)

func ToolNames() []string {
	return append([]string(nil), allRuntimeTools...)
}

// Handlers exposes the full runtime tool suite when allowed is nil, or exactly
// the named subset for a delegated member. Process sessions are scoped to
// owner, so concurrent assignments cannot write to or poll each other's PTYs.
func (r *Runtime) Handlers(
	ctx context.Context,
	owner string,
	allowed []string,
) ([]adk.ChatModelAgentMiddleware, error) {
	if r == nil {
		return nil, fmt.Errorf("tool runtime is required")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("tool owner is required")
	}
	enabled := make(map[string]bool, len(allRuntimeTools))
	if allowed == nil {
		for _, name := range allRuntimeTools {
			enabled[name] = true
		}
	} else {
		supported := make(map[string]struct{}, len(allRuntimeTools))
		for _, name := range allRuntimeTools {
			supported[name] = struct{}{}
		}
		for _, name := range allowed {
			if _, ok := supported[name]; !ok {
				return nil, fmt.Errorf("unsupported runtime tool %q", name)
			}
			enabled[name] = true
		}
	}

	filesystemHandlers, err := r.filesystemHandler(ctx, allowed)
	if err != nil {
		return nil, err
	}
	nativeTools, err := r.nativeTools(owner, enabled)
	if err != nil {
		return nil, err
	}
	handlers := append([]adk.ChatModelAgentMiddleware(nil), filesystemHandlers...)
	if len(nativeTools) > 0 {
		executionPolicy := "exec_command runs inside ALT's fail-closed Linux sandbox: Bubblewrap isolates filesystem mounts, processes, IPC, UTS, and direct networking; no_new_privs and Landlock restrict privilege transitions and writes. It may read the host filesystem, but may write only the session workspace and ALT's private temporary directory."
		if r.options.DangerouslyBypassApprovalsAndSandbox {
			executionPolicy = "DANGEROUS MODE: exec_command bypasses ALT's approval and sandbox boundary and executes directly on the host. Treat every command as having the user's ambient operating-system authority."
		}
		handlers = append(handlers, &toolMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
			instruction:                  executionPolicy + " Running commands return a numeric session_id for write_stdin; copy that exact ID when polling or sending input. An unknown-process lookup does not prove that the process ended: if the ID was transcribed incorrectly, retry with the exact session_id from the latest running result. apply_patch accepts strict Git or standard unified text patches and validates every path against the workspace before changing files. web_search uses the user's separate Exa credential and returns retrieved page text; titles and snippets alone are never evidence. Credentials are never exposed to shell commands.",
			tools:                        nativeTools,
		})
	}
	return handlers, nil
}

func (r *Runtime) nativeTools(owner string, enabled map[string]bool) ([]tool.BaseTool, error) {
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
	if enabled[ToolNameWebSearch] {
		value, err := toolutils.InferTool(
			ToolNameWebSearch,
			"Search and retrieve web evidence through the user's Exa connection, or retrieve exact URLs. Search snippets are leads; retrieved page text is evidence.",
			r.webSearch,
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
