package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"altv1/internal/event"
	"altv1/internal/nativegui"
	"altv1/internal/profile"

	tea "charm.land/bubbletea/v2"
)

type nativeProcess struct {
	command  *exec.Cmd
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	mode     nativegui.Mode
	stopping atomic.Bool
	stdin    io.WriteCloser
	close    sync.Once

	updateMu      sync.Mutex
	updateReady   *sync.Cond
	updateQueue   []event.Event
	updatesClosed bool
}

type nativeStartedMsg struct {
	process *nativeProcess
}

type nativeFinishedMsg struct {
	process   *nativeProcess
	published *nativegui.Published
	err       error
}

func selectedTeamInspectorLaunch(document *profile.Document) nativegui.Launch {
	return nativegui.Launch{
		Mode:      nativegui.ModeTeamInspect,
		ProfileID: document.Profile.ID,
		Revision:  document.Profile.Revision,
	}
}

func launchNativeCmd(
	ctx context.Context,
	dataDir string,
	dangerouslyBypassApprovalsAndSandbox bool,
	launch nativegui.Launch,
) tea.Cmd {
	return func() tea.Msg {
		executable, err := os.Executable()
		if err != nil {
			return errorMsg{fmt.Errorf("resolve ALT executable: %w", err)}
		}
		args := []string{"--data-dir", dataDir}
		if dangerouslyBypassApprovalsAndSandbox {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		args = append(args,
			"__native-gui",
			"--mode", string(launch.Mode),
		)
		if launch.ProfileID != "" {
			args = append(args, "--profile", launch.ProfileID)
		}
		if launch.Revision > 0 {
			args = append(args, "--revision", strconv.Itoa(launch.Revision))
		}
		if launch.SessionID != "" {
			args = append(args, "--session", launch.SessionID)
		}
		process := &nativeProcess{
			command: exec.CommandContext(ctx, executable, args...),
			mode:    launch.Mode,
		}
		if launch.Mode == nativegui.ModeThinking {
			stdin, err := process.command.StdinPipe()
			if err != nil {
				return errorMsg{fmt.Errorf("open thinking event pipe: %w", err)}
			}
			process.stdin = stdin
			process.updateReady = sync.NewCond(&process.updateMu)
		}
		process.command.Stdout = &process.stdout
		process.command.Stderr = &process.stderr
		if err := process.command.Start(); err != nil {
			return errorMsg{fmt.Errorf("open native %s window: %w", launch.Mode, err)}
		}
		if process.updateReady != nil {
			go process.writeEvents()
		}
		return nativeStartedMsg{process: process}
	}
}

func (p *nativeProcess) writeEvents() {
	defer p.stdin.Close()
	encoder := json.NewEncoder(p.stdin)
	for {
		p.updateMu.Lock()
		for len(p.updateQueue) == 0 && !p.updatesClosed {
			p.updateReady.Wait()
		}
		if len(p.updateQueue) == 0 && p.updatesClosed {
			p.updateMu.Unlock()
			return
		}
		item := p.updateQueue[0]
		p.updateQueue[0] = event.Event{}
		p.updateQueue = p.updateQueue[1:]
		p.updateMu.Unlock()
		if err := encoder.Encode(item); err != nil {
			return
		}
	}
}

func (p *nativeProcess) pushEvent(item event.Event) {
	if p == nil || p.updateReady == nil {
		return
	}
	p.updateMu.Lock()
	defer p.updateMu.Unlock()
	if p.updatesClosed {
		return
	}
	p.updateQueue = append(p.updateQueue, item)
	p.updateReady.Signal()
}

func (p *nativeProcess) closeUpdates() {
	if p == nil || p.updateReady == nil {
		return
	}
	p.close.Do(func() {
		p.updateMu.Lock()
		p.updatesClosed = true
		p.updateReady.Broadcast()
		p.updateMu.Unlock()
	})
}

func waitNativeCmd(process *nativeProcess) tea.Cmd {
	return func() tea.Msg {
		err := process.command.Wait()
		if process.stopping.Load() {
			err = nil
		}
		if err != nil {
			detail := strings.TrimSpace(process.stderr.String())
			if detail != "" {
				err = fmt.Errorf("%w: %s", err, detail)
			}
		}
		var result struct {
			Published *nativegui.Published `json:"published"`
		}
		if source := bytes.TrimSpace(process.stdout.Bytes()); len(source) > 0 {
			if decodeErr := json.Unmarshal(source, &result); decodeErr != nil && err == nil {
				err = fmt.Errorf("decode native GUI result: %w", decodeErr)
			}
		}
		return nativeFinishedMsg{
			process: process, published: result.Published, err: err,
		}
	}
}

func stopNativeCmd(process *nativeProcess) tea.Cmd {
	return func() tea.Msg {
		process.stopping.Store(true)
		if process.command.Process == nil {
			return infoMsg("native window is not running")
		}
		if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errorMsg{fmt.Errorf("close native window: %w", err)}
		}
		return infoMsg("closing native window")
	}
}

func parseNativeProfileReference(reference string) (string, int, error) {
	id := reference
	revision := 0
	if profileID, value, ok := strings.Cut(reference, "@"); ok {
		id = profileID
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return "", 0, fmt.Errorf("invalid profile revision")
		}
		revision = parsed
	}
	if strings.TrimSpace(id) == "" {
		return "", 0, fmt.Errorf("profile id is required")
	}
	return id, revision, nil
}

func (m *Model) stopNativeProcesses() {
	for _, process := range []*nativeProcess{m.teamGUI, m.thinkingGUI} {
		if process == nil || process.command.Process == nil {
			continue
		}
		process.stopping.Store(true)
		_ = process.command.Process.Kill()
		process.closeUpdates()
	}
}
