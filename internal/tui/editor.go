package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type editorFinishedMsg struct {
	text string
	err  error
}

type authFinishedMsg struct{ err error }

func externalEditorCmd(draft string) (tea.Cmd, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return nil, fmt.Errorf("set $VISUAL or $EDITOR to use the external editor")
	}

	file, err := os.CreateTemp("", "alt-draft-*.md")
	if err != nil {
		return nil, fmt.Errorf("create draft file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("protect draft file: %w", err)
	}
	if _, err := file.WriteString(draft); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("write draft file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("close draft file: %w", err)
	}

	command := exec.Command("sh", "-c", editor+" \"$1\"", "alt-editor", path)
	return tea.ExecProcess(command, func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return editorFinishedMsg{err: runErr}
		}
		content, readErr := os.ReadFile(path)
		return editorFinishedMsg{text: string(content), err: readErr}
	}), nil
}

func authSetupCmd(gateway string) (tea.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ALT executable: %w", err)
	}
	gateway = strings.TrimSpace(gateway)
	if gateway == "" {
		return nil, fmt.Errorf("gateway is required")
	}
	command := exec.Command(executable, "auth", "set", gateway)
	return tea.ExecProcess(command, func(runErr error) tea.Msg {
		return authFinishedMsg{err: runErr}
	}), nil
}
