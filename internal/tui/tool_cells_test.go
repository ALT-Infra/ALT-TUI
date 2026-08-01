package tui

import (
	"strconv"
	"strings"
	"testing"

	"altv1/internal/event"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestToolCellSemanticStyleContract(t *testing.T) {
	activity := &toolActivity{
		call: event.ToolCallData{
			ToolCallID: "command", Tool: "exec_command",
			Arguments: `{"cmd":"go test ./..."}`,
		},
		completion: &event.ToolCompletedData{
			ToolCallID: "command", Tool: "exec_command",
			Result: `{"output":"ok","running":false,"exit_code":0}`,
		},
	}
	rendered := renderToolActivity(activity, 80, true, false)
	wantMarker := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("•") + " "
	if !strings.HasPrefix(rendered, wantMarker) {
		t.Fatalf("successful command marker is not green and bold: %q", rendered)
	}
	wantVerb := sectionStyle(true).Render("Ran")
	if !strings.Contains(rendered, wantVerb) {
		t.Fatalf("command operation is not bold: %q", rendered)
	}
	highlighted := highlightShell("go test ./...", true)
	if highlighted == "go test ./..." || !strings.Contains(rendered, "\x1b[38;5;") {
		t.Fatalf("command did not pass through the Bash syntax highlighter: %q", rendered)
	}
	if !strings.Contains(rendered, mutedStyle(true).Render("ok")) {
		t.Fatalf("command output is not visually subordinate to its header: %q", rendered)
	}
	if got := ansi.Strip(rendered); !strings.Contains(got, "• Ran go test ./...\n  └ ok") {
		t.Fatalf("styling altered the semantic transcript: %q", got)
	}
}

func TestToolCellStatusAndCategoryStyles(t *testing.T) {
	active := &toolActivity{call: event.ToolCallData{Tool: "web_search", Arguments: `{"query":"ALT"}`}}
	renderedActive := renderToolActivity(active, 80, true, false)
	wantActive := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render("•") + " "
	if !strings.HasPrefix(renderedActive, wantActive) || !strings.Contains(ansi.Strip(renderedActive), "Searching the web") {
		t.Fatalf("active web tool lacks the active semantic style: %q", renderedActive)
	}

	failed := &toolActivity{
		call:       event.ToolCallData{Tool: "grep", Arguments: `{"pattern":"needle"}`},
		completion: &event.ToolCompletedData{Tool: "grep", Failed: true, Error: "permission denied"},
	}
	renderedFailed := renderToolActivity(failed, 80, true, false)
	wantFailed := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render("•") + " "
	if !strings.HasPrefix(renderedFailed, wantFailed) {
		t.Fatalf("failed tool marker is not red and bold: %q", renderedFailed)
	}
	if !strings.Contains(renderedFailed, errorStyle(true).Render("error: permission denied")) {
		t.Fatalf("failed tool detail is not error-styled: %q", renderedFailed)
	}
	wantOperation := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("Search")
	if !strings.Contains(renderedFailed, wantOperation) {
		t.Fatalf("exploration operation is not category-accented: %q", renderedFailed)
	}
}

func TestToolCategoriesRetainDistinctVisualGrammar(t *testing.T) {
	tests := []struct {
		name     string
		activity *toolActivity
		plain    string
		styled   []string
	}{
		{
			name: "generic invocation",
			activity: &toolActivity{
				call:       event.ToolCallData{Tool: "custom_tool", Arguments: `{"scope":"workspace"}`},
				completion: &event.ToolCompletedData{Tool: "custom_tool", Result: `{}`},
			},
			plain: "• Called custom_tool({\"scope\":\"workspace\"})",
			styled: []string{
				lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("custom_tool"),
				mutedStyle(true).Render(`{"scope":"workspace"}`),
			},
		},
		{
			name: "quiet web completion",
			activity: &toolActivity{
				call:       event.ToolCallData{Tool: "web_search", Arguments: `{"query":"graph layout"}`},
				completion: &event.ToolCompletedData{Tool: "web_search", Result: `{}`},
			},
			plain:  "• Searched the web for graph layout",
			styled: []string{lipgloss.NewStyle().Bold(true).Render("Searched the web")},
		},
		{
			name: "quiet file change",
			activity: &toolActivity{
				call:       event.ToolCallData{Tool: "write_file", Arguments: `{"path":"notes.md"}`},
				completion: &event.ToolCompletedData{Tool: "write_file", Result: `{}`},
			},
			plain:  "• Edited notes.md",
			styled: []string{lipgloss.NewStyle().Bold(true).Render("Edited")},
		},
	}
	quietMarker := lipgloss.NewStyle().Bold(true).Faint(true).Render("•") + " "
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendered := renderToolActivity(test.activity, 100, true, false)
			if got := ansi.Strip(rendered); !strings.Contains(got, test.plain) {
				t.Fatalf("plain transcript = %q, want %q", got, test.plain)
			}
			for _, fragment := range test.styled {
				if !strings.Contains(rendered, fragment) {
					t.Fatalf("missing semantic style %q in %q", fragment, rendered)
				}
			}
			if strings.Contains(test.name, "quiet") && !strings.HasPrefix(rendered, quietMarker) {
				t.Fatalf("quiet category rendered a status-colored completion marker: %q", rendered)
			}
		})
	}
}

func TestExpandedPatchUsesDiffSemantics(t *testing.T) {
	activity := &toolActivity{
		call: event.ToolCallData{
			Tool:      "apply_patch",
			Arguments: `{"patch":"--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new"}`,
		},
		completion: &event.ToolCompletedData{Tool: "apply_patch", Result: `{"changed":["file.go"]}`},
	}
	rendered := renderToolActivity(activity, 80, true, true)
	for _, styled := range []string{
		styleDiffLine("@@ -1 +1 @@", true),
		styleDiffLine("-old", true),
		styleDiffLine("+new", true),
	} {
		if !strings.Contains(rendered, styled) {
			t.Fatalf("expanded patch lacks diff styling %q: %q", styled, rendered)
		}
	}
	if got := ansi.Strip(rendered); !strings.Contains(got, "-old") || !strings.Contains(got, "+new") {
		t.Fatalf("diff styling changed patch content: %q", got)
	}
}

func TestToolOutputCannotInjectTerminalControlSequences(t *testing.T) {
	activity := &toolActivity{
		call: event.ToolCallData{Tool: "exec_command", Arguments: `{"cmd":"printf x"}`},
		completion: &event.ToolCompletedData{
			Tool: "exec_command", Result: "{\"output\":\"safe\\u001b[2Jtext \\u001b[31mred\\u001b[0m\",\"running\":false,\"exit_code\":0}",
		},
	}
	rendered := renderToolActivity(activity, 80, true, false)
	if strings.Contains(rendered, "\x1b[2J") {
		t.Fatalf("provider-controlled terminal escape reached the renderer: %q", rendered)
	}
	if !strings.Contains(ansi.Strip(rendered), "safetext") {
		t.Fatalf("sanitization removed ordinary output text: %q", ansi.Strip(rendered))
	}
	if !strings.Contains(rendered, "\x1b[31mred\x1b[0m") {
		t.Fatalf("safe command-output color was not preserved: %q", rendered)
	}
}

func TestShellHighlightSurvivesViewportStartingInsideHeredoc(t *testing.T) {
	command := "python3 - <<'EOF'\n" + strings.Repeat("value = 1\n", 24) + "EOF"
	highlighted := highlightShell(command, true)
	lines := strings.Split(highlighted, "\n")
	if len(lines) < 20 {
		t.Fatalf("highlighted command unexpectedly short: %d lines", len(lines))
	}
	for index := 8; index < 12; index++ {
		if !strings.Contains(lines[index], "\x1b[") {
			t.Fatalf("heredoc line %d depends on ANSI state from an earlier line: %q", index, lines[index])
		}
	}

	activity := &toolActivity{
		call: event.ToolCallData{Tool: "exec_command", Arguments: `{"cmd":` + strconv.Quote(command) + `}`},
		completion: &event.ToolCompletedData{
			Tool: "exec_command", Result: `{"running":false,"exit_code":0}`,
		},
	}
	rendered := renderToolActivity(activity, 80, true, false)
	view := viewport.New(viewport.WithWidth(80), viewport.WithHeight(4))
	view.SetContent(rendered)
	view.SetYOffset(8)
	visible := view.View()
	if !strings.Contains(ansi.Strip(visible), "value = 1") {
		t.Fatalf("viewport did not render the heredoc midpoint: %q", visible)
	}
	if !strings.Contains(visible, "\x1b[") {
		t.Fatalf("viewport midpoint lost syntax highlighting: %q", visible)
	}
}
