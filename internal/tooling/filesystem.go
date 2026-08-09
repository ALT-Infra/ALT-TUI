package tooling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
) ([]tool.BaseTool, string, adk.ChatModelAgentMiddleware, error) {
	enabled := make(map[string]bool, len(filesystemTools))
	for _, name := range filesystemTools {
		enabled[name] = true
	}

	local, err := localbackend.NewBackend(ctx, &localbackend.Config{})
	if err != nil {
		return nil, "", nil, fmt.Errorf("create Eino local tool backend: %w", err)
	}
	backend := &rootedBackend{root: r.root, temp: r.temp, delegate: local}
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
	// SkipClear prevents this guard from rewriting historical context.
	reducer, err := r.toolResultReductionHandler(ctx, backend)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create Eino tool-result reduction middleware: %w", err)
	}
	return configured.Tools, prompt, reducer, nil
}

func (r *Runtime) toolResultReductionHandler(
	ctx context.Context,
	backend reduction.Backend,
) (adk.ChatModelAgentMiddleware, error) {
	return reduction.New(ctx, &reduction.Config{
		Backend:          backend,
		SkipClear:        true,
		ReadFileToolName: filesystemmw.ToolNameReadFile,
		GenTruncOffloadFilePath: func(_ context.Context, detail *reduction.ToolDetail) (string, error) {
			if detail == nil || detail.ToolContext == nil {
				return r.toolOutputPath("tool", ""), nil
			}
			return r.toolOutputPath(detail.ToolContext.Name, detail.ToolContext.CallID), nil
		},
	})
}

func Supported() []string {
	return append([]string(nil), allRuntimeTools...)
}

func toggle(enabled bool) *filesystemmw.ToolConfig {
	return &filesystemmw.ToolConfig{Disable: !enabled}
}

type rootedBackend struct {
	root     string
	temp     string
	delegate adkfilesystem.Backend
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
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	copy := *req
	copy.FilePath = path
	return b.delegate.Write(ctx, &copy)
}

func (b *rootedBackend) Edit(ctx context.Context, req *adkfilesystem.EditRequest) error {
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
	if b.temp == "" {
		return "", fmt.Errorf("private tool-output storage is unavailable")
	}
	relative := strings.TrimPrefix(path, toolOutputPathPrefix)
	if relative == "" {
		return "", fmt.Errorf("invalid private tool-output path")
	}
	storageRoot := filepath.Join(b.temp, "tool-output")
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
