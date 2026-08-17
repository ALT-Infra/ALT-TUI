package tooling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/schema"
)

const toolOutputPathPrefix = "alt-tool-output://"

func IsToolOutputReference(reference string) bool {
	return strings.HasPrefix(strings.TrimSpace(reference), toolOutputPathPrefix)
}

// Runtime owns the model-callable terminal state for one orchestration run.
// Process sessions intentionally survive individual model calls, but are
// terminated when the run ends.
type Runtime struct {
	root       string
	temp       string
	archive    string
	ctx        context.Context
	cancel     context.CancelFunc
	executable string
	processes  *processManager
	options    RuntimeOptions
	outputSeq  atomic.Uint64
	budgetMu   sync.RWMutex
	budgets    map[string]*contextBudget
	closeOnce  sync.Once
	closeErr   error
}

func (r *Runtime) setContextBudget(owner string, budget *contextBudget) {
	if r == nil || budget == nil {
		return
	}
	r.budgetMu.Lock()
	if r.budgets == nil {
		r.budgets = make(map[string]*contextBudget)
	}
	r.budgets[owner] = budget
	r.budgetMu.Unlock()
}

func (r *Runtime) modelVisibleProcessBytes(owner string) int {
	if r == nil {
		return 0
	}
	r.budgetMu.RLock()
	budget := r.budgets[owner]
	r.budgetMu.RUnlock()
	if budget == nil {
		return 0
	}
	// One byte per token is the conservative pre-calibration bound used by the
	// shared context budget. A zero result means the gateway ceiling is still
	// honestly unknown, not that process output is unlimited by policy.
	return budget.hardPromptCapacity(0)
}

func (r *Runtime) modelVisibleResultBytes(owner string) (int, bool) {
	if r == nil {
		return 0, false
	}
	r.budgetMu.RLock()
	budget := r.budgets[owner]
	r.budgetMu.RUnlock()
	if budget == nil {
		return 0, false
	}
	return budget.availableResultBytes()
}

type RuntimeOptions struct {
	DangerouslyBypassApprovalsAndSandbox bool
	SensitiveEnvironment                 []string
	PersistReasoning                     bool
	// ContextArchiveDirectory is durable, private storage for exact tool
	// results removed from the model-visible working context. The orchestrator
	// scopes it to one persisted ALT request. Runtime.Close never removes it.
	ContextArchiveDirectory string
	SearchContext           func(context.Context, string, ContextSearchInput) (ContextSearchResult, error)
	BrowseContext           func(context.Context, string, ContextBrowseInput) (ContextBrowseResult, error)
	OpenContext             func(context.Context, string, ContextOpenInput) (ContextOpenResult, error)
	ArchiveToolOutput       func(context.Context, string, string, []byte) error
	RecordAgentCompaction   func(context.Context, AgentCompactionRecord) error
	ResolveResearchProvider func(context.Context) (string, error)
	ResolveExaCredential    func() (string, error)
	ResolveLinkupCredential func() (string, error)
}

// AgentCompactionRecord explains a lossy working-view transition while its
// TranscriptReference preserves the exact pre-transition evidence. Token
// fields are measurements derived for this call, not user-authored settings.
type AgentCompactionRecord struct {
	Scope               string
	Trigger             string
	TranscriptReference string
	MessagesBefore      int
	MessagesAfter       int
	EstimatedTokens     int
	PromptCapacity      int
	HighWater           int
	Summary             *schema.Message
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
	archive := strings.TrimSpace(options.ContextArchiveDirectory)
	if archive == "" {
		// Direct Runtime users and unit tests retain the historical ephemeral
		// behavior. Production orchestration always supplies a durable path.
		archive = filepath.Join(temp, "context-archive")
	}
	archive, err = filepath.Abs(archive)
	if err != nil {
		os.RemoveAll(temp)
		return nil, fmt.Errorf("resolve context archive directory: %w", err)
	}
	if err := os.MkdirAll(archive, 0o700); err != nil {
		os.RemoveAll(temp)
		return nil, fmt.Errorf("create context archive directory: %w", err)
	}
	if err := os.Chmod(archive, 0o700); err != nil {
		os.RemoveAll(temp)
		return nil, fmt.Errorf("protect context archive directory: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	executable, err := os.Executable()
	if err != nil {
		cancel()
		os.RemoveAll(temp)
		return nil, fmt.Errorf("locate ALT executable: %w", err)
	}
	runtime := &Runtime{
		root: root, temp: temp, archive: archive, ctx: ctx, cancel: cancel,
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
// request's exact context archive. Hashing keeps provider-generated call IDs
// out of filesystem paths; the sequence prevents collisions when a gateway
// omits a call ID. rootedBackend resolves the virtual namespace without
// creating artifacts in the user's workspace.
func (r *Runtime) toolOutputPath(toolName, callID string) string {
	return r.toolOutputPathFor("", toolName, callID)
}

func (r *Runtime) toolOutputPathFor(owner, toolName, callID string) string {
	identity := owner + "\x00" + toolName + "\x00" + callID
	if callID == "" {
		identity += fmt.Sprintf("\x00%d", r.outputSeq.Add(1))
	}
	digest := sha256.Sum256([]byte(identity))
	return toolOutputPathPrefix + hex.EncodeToString(digest[:])
}
