package tooling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"altv1/internal/provider"

	localbackend "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk"
	adkfilesystem "github.com/cloudwego/eino/adk/filesystem"
	filesystemmw "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/components/tool"
)

var filesystemTools = []string{
	filesystemmw.ToolNameLs,
	filesystemmw.ToolNameReadFile,
	filesystemmw.ToolNameWriteFile,
	filesystemmw.ToolNameEditFile,
	filesystemmw.ToolNameGlob,
	filesystemmw.ToolNameGrep,
}

func (r *Runtime) filesystemTools(
	ctx context.Context,
	owner string,
	budget *contextBudget,
) ([]tool.BaseTool, string, adk.ChatModelAgentMiddleware, error) {
	enabled := make(map[string]bool, len(filesystemTools))
	for _, name := range filesystemTools {
		enabled[name] = true
	}

	local, err := localbackend.NewBackend(ctx, &localbackend.Config{})
	if err != nil {
		return nil, "", nil, fmt.Errorf("create Eino local tool backend: %w", err)
	}
	backend := &rootedBackend{root: r.root, archive: r.archive, delegate: local}
	reductionBackend := &rootedBackend{
		root: r.root, archive: r.archive, delegate: local,
		allowArchiveWrite: true, archiveOwner: owner,
		archiveOutput: r.options.ArchiveToolOutput,
	}
	prompt := fmt.Sprintf(
		"Your filesystem workspace is %s. Use relative paths within it. File tools reject lexical paths and symlink resolutions outside this workspace. Only tools made available to this assignment may be called.",
		r.root,
	)
	config := &filesystemmw.MiddlewareConfig{
		Backend:            backend,
		LsToolConfig:       toggle(enabled[filesystemmw.ToolNameLs]),
		ReadFileToolConfig: toggle(enabled[filesystemmw.ToolNameReadFile]),
		WriteFileToolConfig: toggle(
			enabled[filesystemmw.ToolNameWriteFile],
		),
		EditFileToolConfig: toggle(enabled[filesystemmw.ToolNameEditFile]),
		GlobToolConfig:     toggle(enabled[filesystemmw.ToolNameGlob]),
		GrepToolConfig:     toggle(enabled[filesystemmw.ToolNameGrep]),
		CustomSystemPrompt: &prompt,
	}
	handler, err := filesystemmw.New(ctx, config)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create Eino filesystem middleware: %w", err)
	}
	seeded := &adk.ChatModelAgentContext{}
	_, configured, err := handler.BeforeAgent(ctx, seeded)
	if err != nil {
		return nil, "", nil, fmt.Errorf("initialize Eino filesystem tools: %w", err)
	}
	// Eino's reduction middleware preserves a large result in ALT's private
	// runtime storage and replaces the model-visible result with bounded
	// head/tail previews plus an opaque read_file path. This is lossless:
	// read_file's offset/limit contract pages through the complete result.
	// Historical tool rounds are also cleared once the agent loop crosses its
	// working budget. Their exact results are offloaded first, and the retained
	// placeholder remains addressable through read_file. Both paths use the
	// request's durable archive rather than an ephemeral process directory.
	reducer, err := r.toolResultReductionHandler(ctx, owner, reductionBackend, budget)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create Eino tool-result reduction middleware: %w", err)
	}
	return configured.Tools, prompt, reducer, nil
}

func (r *Runtime) toolResultReductionHandler(
	ctx context.Context,
	owner string,
	backend reduction.Backend,
	budgets ...*contextBudget,
) (adk.ChatModelAgentMiddleware, error) {
	budget := newContextBudget(provider.ModelLimits{})
	if len(budgets) > 0 && budgets[0] != nil {
		budget = budgets[0]
	}
	maxVisibleLength := budget.hardPromptCapacity(0)
	if maxVisibleLength <= 0 {
		maxVisibleLength = int(^uint(0) >> 1)
	}
	static, err := reduction.New(ctx, &reduction.Config{
		Backend:          backend,
		SkipClear:        true,
		ReadFileToolName: filesystemmw.ToolNameReadFile,
		// A single result cannot claim more bytes than the model's entire
		// discovered prompt capacity. Unknown capacity disables this immediate
		// truncation; the runtime resource policy remains a separate boundary.
		MaxLengthForTrunc: maxVisibleLength,
		GenTruncOffloadFilePath: func(_ context.Context, detail *reduction.ToolDetail) (string, error) {
			if detail == nil || detail.ToolContext == nil {
				return r.toolOutputPathFor(owner, "tool", ""), nil
			}
			return r.toolOutputPathFor(owner, detail.ToolContext.Name, detail.ToolContext.CallID), nil
		},
		GenClearOffloadFilePath: func(_ context.Context, detail *reduction.ToolDetail) (string, error) {
			if detail == nil || detail.ToolContext == nil {
				return r.toolOutputPathFor(owner, "cleared-tool", ""), nil
			}
			return r.toolOutputPathFor(owner, "cleared:"+detail.ToolContext.Name, detail.ToolContext.CallID), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &adaptiveToolReduction{
		ChatModelAgentMiddleware: static,
		ctx:                      ctx, runtime: r, owner: owner, backend: backend, budget: budget,
	}, nil
}

type adaptiveToolReduction struct {
	adk.ChatModelAgentMiddleware
	ctx     context.Context
	runtime *Runtime
	owner   string
	backend reduction.Backend
	budget  *contextBudget
}

func (m *adaptiveToolReduction) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	modelContext *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil || m == nil || m.budget == nil {
		return ctx, state, nil
	}
	plan := m.budget.plan(state.Messages, state.ToolInfos)
	if !plan.Known || plan.Estimated < plan.HighWater {
		return ctx, state, nil
	}
	clearAtLeast := plan.Estimated - plan.LowWater
	if clearAtLeast <= 0 {
		return ctx, state, nil
	}
	dynamic, err := reduction.New(m.ctx, &reduction.Config{
		Backend:           m.backend,
		SkipTruncation:    true,
		SkipClear:         false,
		ReadFileToolName:  filesystemmw.ToolNameReadFile,
		TokenCounter:      m.budget.summarizationTokenCounter,
		MaxTokensForClear: int64(plan.HighWater),
		// The current completed tool round is a protocol dependency, not an
		// arbitrary history preference. Older rounds are exact-addressable.
		ClearRetentionSuffixLimit: 1,
		// Eviction is applied only when it reaches the calculated low-water
		// mark and can therefore avoid immediate lossy summarization.
		ClearAtLeastTokens: int64(clearAtLeast),
		GenClearOffloadFilePath: func(_ context.Context, detail *reduction.ToolDetail) (string, error) {
			if detail == nil || detail.ToolContext == nil {
				return m.runtime.toolOutputPathFor(m.owner, "cleared-tool", ""), nil
			}
			return m.runtime.toolOutputPathFor(m.owner, "cleared:"+detail.ToolContext.Name, detail.ToolContext.CallID), nil
		},
	})
	if err != nil {
		return ctx, state, err
	}
	return dynamic.BeforeModelRewriteState(ctx, state, modelContext)
}

func toggle(enabled bool) *filesystemmw.ToolConfig {
	return &filesystemmw.ToolConfig{Disable: !enabled}
}

type rootedBackend struct {
	root              string
	archive           string
	delegate          adkfilesystem.Backend
	allowArchiveWrite bool
	archiveOwner      string
	archiveOutput     func(context.Context, string, string, []byte) error
}

func (b *rootedBackend) LsInfo(ctx context.Context, req *adkfilesystem.LsInfoRequest) ([]adkfilesystem.FileInfo, error) {
	path, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	copy := *req
	copy.Path = path
	return b.delegate.LsInfo(ctx, &copy)
}

func (b *rootedBackend) Read(ctx context.Context, req *adkfilesystem.ReadRequest) (*adkfilesystem.FileContent, error) {
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return nil, err
	}
	copy := *req
	copy.FilePath = path
	return b.delegate.Read(ctx, &copy)
}

func (b *rootedBackend) GrepRaw(ctx context.Context, req *adkfilesystem.GrepRequest) ([]adkfilesystem.GrepMatch, error) {
	path, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	copy := *req
	copy.Path = path
	return b.delegate.GrepRaw(ctx, &copy)
}

func (b *rootedBackend) GlobInfo(ctx context.Context, req *adkfilesystem.GlobInfoRequest) ([]adkfilesystem.FileInfo, error) {
	path, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	copy := *req
	copy.Path = path
	return b.delegate.GlobInfo(ctx, &copy)
}

func (b *rootedBackend) Write(ctx context.Context, req *adkfilesystem.WriteRequest) error {
	private := strings.HasPrefix(req.FilePath, toolOutputPathPrefix)
	if private && !b.allowArchiveWrite {
		return fmt.Errorf("private archived tool outputs are immutable")
	}
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	if private {
		content := []byte(req.Content)
		if err := writeImmutableFile(path, content); err != nil {
			return err
		}
		if b.archiveOutput != nil {
			if err := b.archiveOutput(ctx, b.archiveOwner, req.FilePath, content); err != nil {
				return fmt.Errorf("index exact tool output: %w", err)
			}
		}
		return nil
	}
	copy := *req
	copy.FilePath = path
	if err := b.delegate.Write(ctx, &copy); err != nil {
		return err
	}
	return nil
}

func (b *rootedBackend) Edit(ctx context.Context, req *adkfilesystem.EditRequest) error {
	if strings.HasPrefix(req.FilePath, toolOutputPathPrefix) {
		return fmt.Errorf("private archived tool outputs are immutable")
	}
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	copy := *req
	copy.FilePath = path
	return b.delegate.Edit(ctx, &copy)
}

func (b *rootedBackend) resolve(path string) (string, error) {
	if strings.HasPrefix(path, toolOutputPathPrefix) {
		return b.resolveToolOutput(path)
	}
	if strings.TrimSpace(path) == "" {
		return b.root, nil
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(b.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(b.root, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path is outside the session workspace: %s", path)
	}
	resolved, err := resolveExistingAncestor(candidate)
	if err != nil {
		return "", err
	}
	relative, err = filepath.Rel(b.root, resolved)
	if err != nil {
		return "", fmt.Errorf("resolve physical workspace path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path resolves outside the session workspace: %s", path)
	}
	return resolved, nil
}

func (b *rootedBackend) resolveToolOutput(path string) (string, error) {
	if b.archive == "" {
		return "", fmt.Errorf("durable tool-output storage is unavailable")
	}
	relative := strings.TrimPrefix(path, toolOutputPathPrefix)
	if relative == "" {
		return "", fmt.Errorf("invalid private tool-output path")
	}
	storageRoot := filepath.Join(b.archive, "tool-output")
	candidate := filepath.Clean(filepath.Join(storageRoot, filepath.FromSlash(relative)))
	contained, err := filepath.Rel(storageRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve private tool-output path: %w", err)
	}
	if contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) || filepath.IsAbs(contained) {
		return "", fmt.Errorf("invalid private tool-output path")
	}
	return candidate, nil
}

func resolveExistingAncestor(candidate string) (string, error) {
	probe := candidate
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !isPathMissing(err) {
			return "", fmt.Errorf("resolve workspace symlink: %w", err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("resolve workspace path: %w", err)
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func isPathMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
