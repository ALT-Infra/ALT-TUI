package tooling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const toolOutputPathPrefix = "alt-tool-output://"

// Runtime owns the model-callable terminal state for one orchestration run.
// Process sessions intentionally survive individual model calls, but are
// terminated when the run ends.
type Runtime struct {
	root       string
	temp       string
	ctx        context.Context
	cancel     context.CancelFunc
	executable string
	processes  *processManager
	options    RuntimeOptions
	outputSeq  atomic.Uint64
	closeOnce  sync.Once
	closeErr   error
}

type RuntimeOptions struct {
	DangerouslyBypassApprovalsAndSandbox bool
	SensitiveEnvironment                 []string
	ResolveExaCredential                 func() (string, error)
}

func NewRuntime(parent context.Context, workspace string) (*Runtime, error) {
	return NewRuntimeWithOptions(parent, workspace, RuntimeOptions{})
}

func NewRuntimeWithOptions(
	parent context.Context,
	workspace string,
	options RuntimeOptions,
) (*Runtime, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve tool workspace: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve physical tool workspace: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect tool workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tool workspace is not a directory: %s", root)
	}
	temp, err := os.MkdirTemp("", "alt-tool-runtime-*")
	if err != nil {
		return nil, fmt.Errorf("create private tool runtime directory: %w", err)
	}
	if err := os.Chmod(temp, 0o700); err != nil {
		os.RemoveAll(temp)
		return nil, fmt.Errorf("protect private tool runtime directory: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	executable, err := os.Executable()
	if err != nil {
		cancel()
		os.RemoveAll(temp)
		return nil, fmt.Errorf("locate ALT executable: %w", err)
	}
	runtime := &Runtime{
		root: root, temp: temp, ctx: ctx, cancel: cancel,
		executable: executable, options: options,
	}
	runtime.processes = newProcessManager(runtime)
	return runtime, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.cancel()
		r.processes.close()
		r.closeErr = os.RemoveAll(r.temp)
	})
	return r.closeErr
}

func (r *Runtime) Root() string {
	if r == nil {
		return ""
	}
	return r.root
}

// toolOutputPath returns an opaque read_file path for a result stored in the
// runtime's private directory. Hashing keeps provider-generated call IDs out
// of filesystem paths; the sequence prevents collisions when a gateway omits
// a call ID. rootedBackend resolves the virtual namespace without creating
// artifacts in the user's workspace.
func (r *Runtime) toolOutputPath(toolName, callID string) string {
	identity := toolName + "\x00" + callID
	if callID == "" {
		identity += fmt.Sprintf("\x00%d", r.outputSeq.Add(1))
	}
	digest := sha256.Sum256([]byte(identity))
	return toolOutputPathPrefix + hex.EncodeToString(digest[:])
}
