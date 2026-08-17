package tooling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type ExecCommandInput struct {
	Command     string `json:"cmd" jsonschema:"description=Shell command to execute in the session workspace."`
	YieldTimeMS int    `json:"yield_time_ms,omitempty" jsonschema:"description=Optional maximum wait for completion or new output. Zero waits for the next natural process event; use a positive value when a quiet long-running command must yield control."`
	TTY         bool   `json:"tty,omitempty" jsonschema:"description=Allocate a pseudo-terminal for interactive or terminal-sensitive programs."`
}

type WriteStdinInput struct {
	SessionID   int    `json:"session_id" jsonschema:"description=Identifier of the running exec_command session."`
	Chars       string `json:"chars,omitempty" jsonschema:"description=Exact characters to write. Leave empty to poll for output."`
	YieldTimeMS int    `json:"yield_time_ms,omitempty" jsonschema:"description=Optional maximum wait for completion or new output. Zero waits for the next natural process event; use a positive value when polling a quiet long-running process."`
}

type ProcessResult struct {
	SessionID int    `json:"session_id,omitempty"`
	Output    string `json:"output,omitempty"`
	Running   bool   `json:"running"`
	HasMore   bool   `json:"has_more,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

type processManager struct {
	runtime  *Runtime
	mu       sync.Mutex
	sessions map[int]*processSession
	reserved map[int]struct{}
	nextID   int
	closed   bool
}

type processSession struct {
	id           int
	owner        string
	command      *exec.Cmd
	output       *os.File
	stdin        io.Writer
	ptmx         *os.File
	done         chan struct{}
	copyDone     chan struct{}
	outputNotify chan struct{}
	readMu       sync.Mutex
	writeMu      sync.Mutex
	offset       int64
	stateMu      sync.Mutex
	exitCode     *int
	waitErr      error
}

func newProcessManager(runtime *Runtime) *processManager {
	return &processManager{
		runtime:  runtime,
		sessions: make(map[int]*processSession),
		reserved: make(map[int]struct{}),
	}
}

func (m *processManager) start(
	ctx context.Context,
	owner string,
	input ExecCommandInput,
) (ProcessResult, error) {
	commandText := strings.TrimSpace(input.Command)
	if commandText == "" {
		return ProcessResult{}, fmt.Errorf("cmd is required")
	}
	yield, err := processYield(input.YieldTimeMS)
	if err != nil {
		return ProcessResult{}, err
	}
	sessionID, err := m.reserveSessionID()
	if err != nil {
		return ProcessResult{}, err
	}
	registered := false
	defer func() {
		if !registered {
			m.releaseSessionID(sessionID)
		}
	}()
	output, err := os.OpenFile(
		fmt.Sprintf("%s/output-%d", m.runtime.temp, sessionID),
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("create process transcript: %w", err)
	}
	args := []string{
		"__tool-exec",
		"--workspace", m.runtime.root,
		"--temp", m.runtime.temp,
		"--",
		"/bin/sh", "-lc", input.Command,
	}
	var command *exec.Cmd
	if m.runtime.options.DangerouslyBypassApprovalsAndSandbox {
		command = exec.CommandContext(m.runtime.ctx, "/bin/sh", "-lc", input.Command)
	} else {
		command = exec.CommandContext(m.runtime.ctx, m.runtime.executable, args...)
	}
	command.Dir = m.runtime.root
	command.Env = append(
		sanitizedEnvironment(m.runtime.options.SensitiveEnvironment),
		"TMPDIR="+m.runtime.temp,
		"TEMP="+m.runtime.temp,
		"TMP="+m.runtime.temp,
	)
	session := &processSession{
		id: sessionID, owner: owner, command: command, output: output,
		done: make(chan struct{}), copyDone: make(chan struct{}),
		outputNotify: make(chan struct{}, 1),
	}
	capture := &processCapture{session: session}
	if input.TTY {
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		ptmx, startErr := pty.StartWithAttrs(command, nil, command.SysProcAttr)
		if startErr != nil {
			output.Close()
			os.Remove(output.Name())
			return ProcessResult{}, fmt.Errorf("start terminal command: %w", startErr)
		}
		session.ptmx = ptmx
		session.stdin = ptmx
		go func() {
			defer close(session.copyDone)
			_, _ = io.Copy(capture, ptmx)
		}()
	} else {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = capture
		command.Stderr = capture
		stdin, pipeErr := command.StdinPipe()
		if pipeErr != nil {
			output.Close()
			os.Remove(output.Name())
			return ProcessResult{}, fmt.Errorf("create command input pipe: %w", pipeErr)
		}
		session.stdin = stdin
		if startErr := command.Start(); startErr != nil {
			output.Close()
			os.Remove(output.Name())
			return ProcessResult{}, fmt.Errorf("start command: %w", startErr)
		}
		close(session.copyDone)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		session.terminate()
		output.Close()
		os.Remove(output.Name())
		return ProcessResult{}, fmt.Errorf("tool runtime is closed")
	}
	m.sessions[sessionID] = session
	registered = true
	m.mu.Unlock()
	go session.wait()
	return m.collect(ctx, owner, session, yield)
}

func sanitizedEnvironment(sensitive []string) []string {
	blocked := map[string]struct{}{
		"SSH_AUTH_SOCK":            {},
		"DBUS_SESSION_BUS_ADDRESS": {},
		"DISPLAY":                  {},
		"WAYLAND_DISPLAY":          {},
		"ALL_PROXY":                {},
		"HTTP_PROXY":               {},
		"HTTPS_PROXY":              {},
		"NO_PROXY":                 {},
		"all_proxy":                {},
		"http_proxy":               {},
		"https_proxy":              {},
		"no_proxy":                 {},
	}
	for _, name := range sensitive {
		name = strings.TrimSpace(name)
		if name != "" {
			blocked[name] = struct{}{}
		}
	}
	values := os.Environ()
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if _, remove := blocked[name]; remove {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func (m *processManager) write(
	ctx context.Context,
	owner string,
	input WriteStdinInput,
) (ProcessResult, error) {
	sessionID := input.SessionID
	if sessionID == 0 {
		return ProcessResult{}, fmt.Errorf("session_id is required")
	}
	yield, err := processYield(input.YieldTimeMS)
	if err != nil {
		return ProcessResult{}, err
	}
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil || session.owner != owner {
		return ProcessResult{}, unknownProcessError(sessionID)
	}
	if input.Chars != "" {
		select {
		case <-session.done:
			return m.collect(ctx, owner, session, 0)
		default:
		}
		if _, err := io.WriteString(session.stdin, input.Chars); err != nil {
			return ProcessResult{}, fmt.Errorf("write process input: %w", err)
		}
	}
	return m.collect(ctx, owner, session, yield)
}

func (m *processManager) collect(
	ctx context.Context,
	owner string,
	session *processSession,
	yield time.Duration,
) (ProcessResult, error) {
	if session.owner != owner {
		return ProcessResult{}, unknownProcessError(session.id)
	}
	if yield > 0 {
		timer := time.NewTimer(yield)
		defer timer.Stop()
		select {
		case <-session.done:
		case <-session.outputNotify:
		case <-timer.C:
		case <-ctx.Done():
			return ProcessResult{}, ctx.Err()
		}
	} else if yield < 0 {
		select {
		case <-session.done:
		case <-session.outputNotify:
		case <-ctx.Done():
			return ProcessResult{}, ctx.Err()
		}
	}
	output, hasMore, err := session.readNew(m.runtime.modelVisibleProcessBytes(owner))
	if err != nil {
		return ProcessResult{}, err
	}
	select {
	case <-session.done:
		session.stateMu.Lock()
		exitCode := session.exitCode
		waitErr := session.waitErr
		session.stateMu.Unlock()
		if waitErr != nil && exitCode == nil {
			return ProcessResult{}, waitErr
		}
		if hasMore {
			return ProcessResult{
				SessionID: session.id, Output: output, Running: false,
				HasMore: true, ExitCode: exitCode,
			}, nil
		}
		m.mu.Lock()
		delete(m.sessions, session.id)
		delete(m.reserved, session.id)
		m.mu.Unlock()
		session.cleanup()
		return ProcessResult{Output: output, ExitCode: exitCode}, nil
	default:
		return ProcessResult{
			SessionID: session.id,
			Output:    output,
			Running:   true,
			HasMore:   hasMore,
		}, nil
	}
}

type processCapture struct {
	session *processSession
}

func (w *processCapture) Write(content []byte) (int, error) {
	w.session.writeMu.Lock()
	count, err := w.session.output.Write(content)
	w.session.writeMu.Unlock()
	if count > 0 {
		select {
		case w.session.outputNotify <- struct{}{}:
		default:
		}
	}
	return count, err
}

func (s *processSession) wait() {
	err := s.command.Wait()
	<-s.copyDone
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
	var exitCode *int
	if s.command.ProcessState != nil {
		code := s.command.ProcessState.ExitCode()
		exitCode = &code
	}
	s.stateMu.Lock()
	s.exitCode = exitCode
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			s.waitErr = fmt.Errorf("wait for command: %w", err)
		}
	}
	s.stateMu.Unlock()
	close(s.done)
}

func (s *processSession) readNew(limit int) (string, bool, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	info, err := s.output.Stat()
	if err != nil {
		return "", false, fmt.Errorf("inspect process transcript: %w", err)
	}
	if info.Size() <= s.offset {
		return "", false, nil
	}
	available := info.Size() - s.offset
	readSize := available
	if limit > 0 && readSize > int64(limit) {
		readSize = int64(limit)
	}
	content := make([]byte, readSize)
	count, err := s.output.ReadAt(content, s.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("read process transcript: %w", err)
	}
	s.offset += int64(count)
	return string(content[:count]), s.offset < info.Size(), nil
}

func (s *processSession) terminate() {
	if s.command == nil || s.command.Process == nil {
		return
	}
	_ = syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL)
	_ = s.command.Process.Kill()
}

func (s *processSession) cleanup() {
	_ = s.output.Close()
	_ = os.Remove(s.output.Name())
}

// reserveSessionID issues the next small positive handle in this private
// runtime. Authority comes from the owner check, so randomness and a copied
// numeric range add no safety; sequential allocation also cannot collide.
func (m *processManager) reserveSessionID() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, fmt.Errorf("tool runtime is closed")
	}
	for {
		if m.nextID == int(^uint(0)>>1) {
			m.nextID = 0
		}
		m.nextID++
		candidate := m.nextID
		if _, exists := m.reserved[candidate]; exists {
			continue
		}
		m.reserved[candidate] = struct{}{}
		return candidate, nil
	}
}

func (m *processManager) releaseSessionID(sessionID int) {
	m.mu.Lock()
	delete(m.sessions, sessionID)
	delete(m.reserved, sessionID)
	m.mu.Unlock()
}

func unknownProcessError(sessionID int) error {
	// Missing IDs and IDs owned by another assignment intentionally have the
	// same result, so the process namespace cannot be enumerated across owners.
	return fmt.Errorf("unknown process id %d for this assignment", sessionID)
}

func (m *processManager) close() {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*processSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[int]*processSession)
	m.reserved = make(map[int]struct{})
	m.mu.Unlock()
	for _, session := range sessions {
		session.terminate()
		<-session.done
		session.cleanup()
	}
}

func processYield(milliseconds int) (time.Duration, error) {
	if milliseconds < 0 {
		return 0, fmt.Errorf("yield_time-ms cannot be negative")
	}
	if milliseconds == 0 {
		// Negative is an internal sentinel for event-driven waiting. Zero is
		// retained for the explicit non-blocking collection used after done.
		return -1, nil
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}
