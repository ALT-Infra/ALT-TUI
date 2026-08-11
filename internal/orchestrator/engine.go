package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"
)

type Engine struct {
	store     *store.Store
	providers *provider.Registry
	options   EngineOptions
	mu        sync.Mutex
	runs      map[string]*Run
}

type EngineOptions struct {
	DangerouslyBypassApprovalsAndSandbox bool
	SensitiveEnvironment                 []string
	ContextArchiveRoot                   string
	ResolveResearchProvider              func(context.Context) (string, error)
	ResolveExaCredential                 func() (string, error)
	ResolveLinkupCredential              func() (string, error)
}

type Run struct {
	SessionID      string
	ConversationID string
	Workspace      string
	done           chan struct{}
	cancel         context.CancelFunc
	userCancelled  atomic.Bool
	finalizing     atomic.Bool
	runtime        *sessionRuntime
	mu             sync.Mutex
	err            error
}

func NewEngine(ledger *store.Store, providers *provider.Registry) *Engine {
	return NewEngineWithOptions(ledger, providers, EngineOptions{})
}

func NewEngineWithOptions(
	ledger *store.Store,
	providers *provider.Registry,
	options EngineOptions,
) *Engine {
	return &Engine{
		store:     ledger,
		providers: providers,
		options:   options,
		runs:      make(map[string]*Run),
	}
}

func (e *Engine) Start(ctx context.Context, document *profile.Document, task string) (*Run, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve session workspace: %w", err)
	}
	return e.StartAt(ctx, document, task, workspace)
}

func (e *Engine) StartAt(
	ctx context.Context,
	document *profile.Document,
	task string,
	workspace string,
) (*Run, error) {
	if task == "" {
		return nil, fmt.Errorf("task cannot be empty")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve session workspace: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, fmt.Errorf("inspect session workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session workspace is not a directory: %s", workspace)
	}
	if err := e.providers.ValidateProfile(ctx, document.Profile); err != nil {
		return nil, err
	}
	if err := e.store.ImportProfile(ctx, document); err != nil {
		return nil, err
	}
	session, err := e.store.CreateSession(ctx, document, task, workspace)
	if err != nil {
		return nil, err
	}
	return e.startSession(ctx, session, document, false)
}

func (e *Engine) Resume(ctx context.Context, sessionID string) (*Run, error) {
	session, err := e.store.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.Status != store.SessionRunning {
		return nil, fmt.Errorf("session %s is %s and cannot be resumed", sessionID, session.Status)
	}
	document, err := e.store.Profile(ctx, session.ProfileID, session.ProfileRevision)
	if err != nil {
		return nil, err
	}
	if document.Digest != session.ProfileDigest {
		return nil, fmt.Errorf("session profile digest mismatch")
	}
	if err := e.providers.ValidateProfile(ctx, document.Profile); err != nil {
		return nil, err
	}
	return e.startSession(ctx, session, document, true)
}

// Continue starts another adaptive Lead turn in the same durable
// conversation. Each turn keeps its own event stream while sharing the
// conversation's pinned Team Profile, workspace, title, and transcript.
func (e *Engine) Continue(ctx context.Context, previousSessionID, task string) (*Run, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, fmt.Errorf("task cannot be empty")
	}
	previous, err := e.store.Session(ctx, previousSessionID)
	if err != nil {
		return nil, err
	}
	if previous.Status == store.SessionRunning {
		return nil, fmt.Errorf("session %s is still running", previousSessionID)
	}
	document, err := e.store.Profile(ctx, previous.ProfileID, previous.ProfileRevision)
	if err != nil {
		return nil, err
	}
	if document.Digest != previous.ProfileDigest {
		return nil, fmt.Errorf("session profile digest mismatch")
	}
	if err := e.providers.ValidateProfile(ctx, document.Profile); err != nil {
		return nil, err
	}
	session, err := e.store.CreateContinuation(ctx, previousSessionID, document, task)
	if err != nil {
		return nil, err
	}
	return e.startSession(ctx, session, document, false)
}

func (e *Engine) ResumeAll(ctx context.Context) ([]*Run, error) {
	sessions, err := e.store.RecoverableSessions(ctx)
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for i := range sessions {
		run, err := e.Resume(ctx, sessions[i].ID)
		if err != nil {
			return runs, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (e *Engine) startSession(ctx context.Context, session *store.Session, document *profile.Document, recovered bool) (*Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing := e.runs[session.ID]; existing != nil {
		return existing, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &Run{
		SessionID: session.ID, ConversationID: session.ConversationID,
		Workspace: session.Workspace,
		done:      make(chan struct{}), cancel: cancel,
	}
	runtime := newSessionRuntime(e.store, e.providers, session, document, run)
	runtime.engineOptions = e.options
	run.runtime = runtime
	e.runs[session.ID] = run
	go func() {
		defer close(run.done)
		err := runtime.execute(runCtx, recovered)
		run.mu.Lock()
		run.err = err
		run.mu.Unlock()
		e.mu.Lock()
		delete(e.runs, session.ID)
		e.mu.Unlock()
	}()
	return run, nil
}

func (e *Engine) Steer(ctx context.Context, sessionID, instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("instruction cannot be empty")
	}
	e.mu.Lock()
	run := e.runs[sessionID]
	e.mu.Unlock()
	if run == nil {
		return fmt.Errorf("session is not running")
	}
	if run.finalizing.Load() {
		return fmt.Errorf("the Lead is already finalizing")
	}
	item, err := e.store.Append(ctx, sessionID, event.Draft{
		Kind:  event.UserInstruction,
		Actor: "user",
		Data:  event.UserInstructionData{Text: instruction},
	})
	if err != nil {
		return err
	}
	if !run.runtime.pushSignal(Signal{
		Kind:    string(event.UserInstruction),
		EventID: item.ID,
	}) {
		return fmt.Errorf("instruction was saved, but the Lead stopped before it could observe it")
	}
	return nil
}

func (e *Engine) Cancel(ctx context.Context, sessionID, reason string) error {
	e.mu.Lock()
	run := e.runs[sessionID]
	e.mu.Unlock()
	if run == nil {
		session, err := e.store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.Status != store.SessionRunning {
			return nil
		}
		_, err = e.store.Append(ctx, sessionID, event.Draft{
			Kind: event.SessionCancelled, Actor: "user",
			Data: event.FailureData{Error: reason},
		})
		return err
	}
	run.userCancelled.Store(true)
	_, err := e.store.Append(ctx, sessionID, event.Draft{
		Kind: event.SessionCancelled, Actor: "user",
		Data: event.FailureData{Error: reason},
	})
	run.cancel()
	return err
}

func (e *Engine) Active(sessionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[sessionID] != nil
}

func (r *Run) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err
	}
}

func (r *Run) Err() error {
	select {
	case <-r.done:
	default:
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func terminalContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
