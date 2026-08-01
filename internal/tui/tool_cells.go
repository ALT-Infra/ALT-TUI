package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"altv1/internal/event"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

const toolTimelinePrefix = "@tool:"

var safeToolSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

var shellSyntaxThemes struct {
	once  sync.Once
	dark  *chroma.Style
	light *chroma.Style
}

type toolActivity struct {
	actor      string
	call       event.ToolCallData
	completion *event.ToolCompletedData
	output     []string
}

type toolStatus uint8

const (
	toolActive toolStatus = iota
	toolSucceeded
	toolSucceededQuiet
	toolFailed
)

type toolTitleKind uint8

const (
	toolTitlePlain toolTitleKind = iota
	toolTitleCommand
	toolTitlePatch
	toolTitleInvocation
)

type toolLineKind uint8

const (
	toolLineDetail toolLineKind = iota
	toolLineOperation
	toolLineOutput
	toolLineError
	toolLineDiff
)

type toolCellLine struct {
	text string
	kind toolLineKind
}

type toolCell struct {
	verb      string
	subject   string
	titleKind toolTitleKind
	status    toolStatus
	lines     []toolCellLine
}

func (turn *turnView) recordToolCall(actor string, call event.ToolCallData) {
	if turn.toolActivities == nil {
		turn.toolActivities = make(map[string]*toolActivity)
	}
	if turn.processTools == nil {
		turn.processTools = make(map[string]string)
	}
	key := call.ToolCallID
	if key == "" {
		key = fmt.Sprintf("%s:%d", call.Tool, len(turn.timeline))
	}
	if call.Tool == "write_stdin" {
		sessionID := identifierValue(decodeObject(call.Arguments), "session_id")
		if originalKey := turn.processTools[sessionID]; originalKey != "" {
			if original := turn.toolActivities[originalKey]; original != nil {
				turn.toolActivities[key] = original
				return
			}
		}
	}
	turn.toolActivities[key] = &toolActivity{actor: actor, call: call}
	turn.timeline = append(turn.timeline, toolTimelinePrefix+key)
}

func (turn *turnView) recordToolCompletion(actor string, completion event.ToolCompletedData) {
	if turn.toolActivities == nil {
		turn.toolActivities = make(map[string]*toolActivity)
	}
	if turn.processTools == nil {
		turn.processTools = make(map[string]string)
	}
	key := completion.ToolCallID
	if activity := turn.toolActivities[key]; key != "" && activity != nil {
		copy := completion
		activity.completion = &copy
		activity.captureProcessResult(completion.Result)
		if sessionID := identifierValue(decodeObject(completion.Result), "session_id"); sessionID != "" {
			turn.processTools[sessionID] = key
		}
		return
	}
	if key == "" {
		key = fmt.Sprintf("%s:completion:%d", completion.Tool, len(turn.timeline))
	}
	copy := completion
	turn.toolActivities[key] = &toolActivity{
		actor: actor,
		call: event.ToolCallData{
			ToolCallID: completion.ToolCallID,
			Tool:       completion.Tool,
		},
		completion: &copy,
	}
	turn.toolActivities[key].captureProcessResult(completion.Result)
	turn.timeline = append(turn.timeline, toolTimelinePrefix+key)
}

func (activity *toolActivity) captureProcessResult(result string) {
	if activity == nil {
		return
	}
	if output := stringValue(decodeObject(result), "output"); output != "" {
		activity.output = append(activity.output, output)
	}
}

func renderToolActivity(activity *toolActivity, width int, dark, expanded bool) string {
	if activity == nil {
		return ""
	}
	tool := strings.TrimSpace(activity.call.Tool)
	switch tool {
	case "exec_command", "write_stdin":
		return renderCommandTool(activity, width, dark, expanded)
	case "apply_patch":
		return renderPatchTool(activity, width, dark, expanded)
	case "ls", "read_file", "glob", "grep":
		return renderExplorationTool(activity, width, dark, expanded)
	case "write_file", "edit_file":
		return renderFileMutationTool(activity, width, dark, expanded)
	case "web_search":
		return renderWebTool(activity, width, dark, expanded)
	default:
		return renderGenericTool(activity, width, dark, expanded)
	}
}

func renderCommandTool(activity *toolActivity, width int, dark, expanded bool) string {
	arguments := decodeObject(activity.call.Arguments)
	command := stringValue(arguments, "cmd")
	if command == "" {
		command = stringValue(arguments, "chars")
	}
	if command == "" {
		command = activity.call.Tool
	}
	status := toolActivityStatus(activity)
	verb := "Running"
	if status != toolActive {
		verb = "Ran"
	}
	cell := toolCell{verb: verb, subject: command, titleKind: toolTitleCommand, status: status}
	if activity.completion == nil {
		return renderToolCell(cell, width, dark)
	}
	result := decodeObject(activity.completion.Result)
	output := strings.Join(activity.output, "")
	if output == "" {
		output = stringValue(result, "output")
	}
	if output == "" && expanded {
		output = activity.completion.Result
	}
	exit := intValue(result, "exit_code")
	if activity.completion.Failed {
		detail := firstNonEmptyTUI(activity.completion.Error, output, "tool failed")
		cell.lines = append(cell.lines, toolCellLine{text: "error: " + detail, kind: toolLineError})
	} else if output != "" {
		cell.appendToolLines(visibleOutputLines(output, expanded), toolLineOutput)
	} else if status != toolActive {
		cell.lines = append(cell.lines, toolCellLine{text: "(no output)", kind: toolLineOutput})
	}
	if exit != nil && *exit != 0 {
		cell.subject += " (exit " + strconv.Itoa(*exit) + ")"
	}
	return renderToolCell(cell, width, dark)
}

func renderPatchTool(activity *toolActivity, width int, dark, expanded bool) string {
	arguments := decodeObject(activity.call.Arguments)
	patch := stringValue(arguments, "patch")
	stats := patchFileStats(patch)
	paths := sortedStatPaths(stats)
	if activity.completion != nil {
		result := decodeObject(activity.completion.Result)
		if changed, ok := result["changed"].([]any); ok && len(changed) > 0 {
			paths = paths[:0]
			for _, raw := range changed {
				if path, ok := raw.(string); ok {
					paths = append(paths, path)
				}
			}
			sort.Strings(paths)
		}
	}
	status := toolActivityStatus(activity)
	verb := "Editing"
	if status != toolActive {
		verb = "Edited"
	}
	if status == toolFailed {
		verb = "Failed to apply patch"
	}
	cell := toolCell{verb: verb, titleKind: toolTitlePatch, status: quietSuccess(status)}
	if len(paths) == 1 {
		stat := stats[paths[0]]
		cell.subject = fmt.Sprintf("%s (+%d -%d)", paths[0], stat.added, stat.removed)
		if expanded && patch != "" {
			cell.appendToolLines(visibleOutputLines(patch, true), toolLineDiff)
		}
		cell.appendFailure(activity)
		return renderToolCell(cell, width, dark)
	}
	cell.subject = fmt.Sprintf("%d files", len(paths))
	for _, path := range paths {
		stat := stats[path]
		cell.lines = append(cell.lines, toolCellLine{
			text: fmt.Sprintf("%s (+%d -%d)", path, stat.added, stat.removed),
			kind: toolLineDetail,
		})
	}
	if expanded && patch != "" {
		cell.appendToolLines(visibleOutputLines(patch, true), toolLineDiff)
	}
	cell.appendFailure(activity)
	return renderToolCell(cell, width, dark)
}

func renderExplorationTool(activity *toolActivity, width int, dark, expanded bool) string {
	arguments := decodeObject(activity.call.Arguments)
	verb := map[string]string{
		"ls": "List", "read_file": "Read", "glob": "Glob", "grep": "Search",
	}[activity.call.Tool]
	detail := firstNonEmptyTUI(
		stringValue(arguments, "path"),
		stringValue(arguments, "pattern"),
		stringValue(arguments, "query"),
	)
	if detail == "" {
		detail = compactJSON(activity.call.Arguments)
	}
	status := toolActivityStatus(activity)
	title := "Exploring"
	if status != toolActive {
		title = "Explored"
	}
	cell := toolCell{verb: title, status: quietSuccess(status)}
	cell.lines = append(cell.lines, toolCellLine{text: strings.TrimSpace(verb + " " + detail), kind: toolLineOperation})
	if expanded && activity.completion != nil && activity.completion.Result != "" {
		cell.appendToolLines(visibleOutputLines(activity.completion.Result, true), toolLineOutput)
	}
	cell.appendFailure(activity)
	return renderToolCell(cell, width, dark)
}

func renderFileMutationTool(activity *toolActivity, width int, dark, expanded bool) string {
	arguments := decodeObject(activity.call.Arguments)
	path := firstNonEmptyTUI(stringValue(arguments, "path"), stringValue(arguments, "file_path"))
	status := toolActivityStatus(activity)
	verb := "Editing"
	if status != toolActive {
		verb = "Edited"
	}
	cell := toolCell{verb: verb, subject: path, status: quietSuccess(status)}
	if expanded && activity.completion != nil && activity.completion.Result != "" {
		cell.appendToolLines(visibleOutputLines(activity.completion.Result, true), toolLineOutput)
	}
	cell.appendFailure(activity)
	return renderToolCell(cell, width, dark)
}

func renderWebTool(activity *toolActivity, width int, dark, expanded bool) string {
	arguments := decodeObject(activity.call.Arguments)
	detail := stringValue(arguments, "query")
	if detail == "" {
		if urls, ok := arguments["urls"].([]any); ok {
			values := make([]string, 0, len(urls))
			for _, raw := range urls {
				if value, ok := raw.(string); ok {
					values = append(values, value)
				}
			}
			detail = strings.Join(values, ", ")
		}
	}
	status := toolActivityStatus(activity)
	verb := "Searching the web"
	if status != toolActive {
		verb = "Searched the web"
	}
	cell := toolCell{verb: verb, status: quietSuccess(status)}
	if detail != "" && status == toolSucceeded {
		cell.subject = "for " + detail
	} else {
		cell.subject = detail
	}
	if expanded && activity.completion != nil && activity.completion.Result != "" {
		cell.appendToolLines(visibleOutputLines(activity.completion.Result, true), toolLineOutput)
	}
	cell.appendFailure(activity)
	return renderToolCell(cell, width, dark)
}

func renderGenericTool(activity *toolActivity, width int, dark, expanded bool) string {
	status := toolActivityStatus(activity)
	verb := "Calling"
	if status != toolActive {
		verb = "Called"
	}
	cell := toolCell{verb: verb, subject: activity.call.Tool, titleKind: toolTitleInvocation, status: status}
	if arguments := compactJSON(activity.call.Arguments); arguments != "" {
		cell.subject += "(" + arguments + ")"
	}
	if expanded && activity.completion != nil && activity.completion.Result != "" {
		cell.appendToolLines(visibleOutputLines(activity.completion.Result, true), toolLineOutput)
	}
	cell.appendFailure(activity)
	return renderToolCell(cell, width, dark)
}

func toolActivityStatus(activity *toolActivity) toolStatus {
	if activity == nil || activity.completion == nil {
		return toolActive
	}
	if activity.completion.Failed {
		return toolFailed
	}
	result := decodeObject(activity.completion.Result)
	if boolValue(result, "running") {
		return toolActive
	}
	if exit := intValue(result, "exit_code"); exit != nil && *exit != 0 {
		return toolFailed
	}
	return toolSucceeded
}

func quietSuccess(status toolStatus) toolStatus {
	if status == toolSucceeded {
		return toolSucceededQuiet
	}
	return status
}

func (cell *toolCell) appendToolLines(lines []string, kind toolLineKind) {
	for _, line := range lines {
		cell.lines = append(cell.lines, toolCellLine{text: line, kind: kind})
	}
}

func (cell *toolCell) appendFailure(activity *toolActivity) {
	if cell.status != toolFailed || activity == nil || activity.completion == nil {
		return
	}
	detail := firstNonEmptyTUI(activity.completion.Error, activity.completion.Result, "tool failed")
	cell.lines = append(cell.lines, toolCellLine{text: "error: " + detail, kind: toolLineError})
}

func renderToolCell(cell toolCell, width int, dark bool) string {
	cell.verb = strings.TrimSpace(sanitizeTerminalText(cell.verb))
	cell.subject = strings.TrimSpace(sanitizeTerminalText(cell.subject))
	if cell.verb == "" {
		return ""
	}
	title := sectionStyle(dark).Render(cell.verb)
	if cell.subject != "" {
		subject := cell.subject
		switch cell.titleKind {
		case toolTitleCommand:
			subject = highlightShell(subject, dark)
		case toolTitlePatch:
			subject = stylePatchSummary(subject)
		case toolTitleInvocation:
			subject = styleToolInvocation(subject, dark)
		}
		title += " " + subject
	}
	lines := []toolCellLine{{text: title, kind: toolLineDetail}}
	for _, line := range cell.lines {
		if line.kind == toolLineOutput {
			line.text = sanitizeToolOutput(line.text)
		} else {
			line.text = sanitizeTerminalText(line.text)
		}
		line.text = strings.TrimRight(line.text, "\r\n")
		if line.text != "" {
			lines = append(lines, line)
		}
	}
	var output []string
	for index, line := range lines {
		initial := "  └ "
		continuation := "    "
		if index == 0 {
			initial = toolStatusMarker(cell.status)
			continuation = "  "
		} else if index < len(lines)-1 {
			initial = "  │ "
		}
		value := line.text
		if index > 0 {
			value = styleToolDetail(value, line.kind, dark)
		}
		plainPrefixWidth := ansi.StringWidth(initial)
		wrapped := strings.Split(ansi.Wrap(value, max(1, width-plainPrefixWidth), " "), "\n")
		for lineIndex, wrappedLine := range wrapped {
			prefix := continuation
			if lineIndex == 0 {
				prefix = initial
			}
			if index > 0 {
				prefix = mutedStyle(dark).Render(prefix)
			}
			output = append(output, prefix+wrappedLine)
		}
	}
	return strings.Join(output, "\n")
}

func toolStatusMarker(status toolStatus) string {
	style := lipgloss.NewStyle().Bold(true)
	switch status {
	case toolActive:
		style = style.Foreground(lipgloss.Color("6"))
	case toolSucceeded:
		style = style.Foreground(lipgloss.Color("2"))
	case toolSucceededQuiet:
		style = style.Faint(true)
	case toolFailed:
		style = style.Foreground(lipgloss.Color("1"))
	}
	return style.Render("•") + " "
}

func stylePatchSummary(value string) string {
	start := strings.LastIndex(value, " (+")
	if start < 0 || !strings.HasSuffix(value, ")") {
		return value
	}
	stats := value[start+2 : len(value)-1]
	parts := strings.Fields(stats)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "+") || !strings.HasPrefix(parts[1], "-") {
		return value
	}
	return value[:start+2] +
		lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(parts[0]) + " " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(parts[1]) + ")"
}

func styleToolDetail(value string, kind toolLineKind, dark bool) string {
	switch kind {
	case toolLineOperation:
		verb, rest, _ := strings.Cut(value, " ")
		accent := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(verb)
		if rest == "" {
			return accent
		}
		return accent + " " + rest
	case toolLineError:
		return errorStyle(dark).Render(value)
	case toolLineDiff:
		return styleDiffLine(value, dark)
	default:
		return mutedStyle(dark).Render(value)
	}
}

func styleToolInvocation(value string, dark bool) string {
	open := strings.IndexByte(value, '(')
	if open < 0 || !strings.HasSuffix(value, ")") {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(value)
	}
	tool := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(value[:open])
	arguments := mutedStyle(dark).Render(value[open+1 : len(value)-1])
	return tool + "(" + arguments + ")"
}

func styleDiffLine(value string, dark bool) string {
	switch {
	case strings.HasPrefix(value, "@@"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(value)
	case strings.HasPrefix(value, "+") && !strings.HasPrefix(value, "+++"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(value)
	case strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "---"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(value)
	default:
		return mutedStyle(dark).Render(value)
	}
}

func highlightShell(command string, dark bool) string {
	lexer := lexers.Get("bash")
	if lexer == nil {
		return command
	}
	iterator, err := lexer.Tokenise(nil, command)
	if err != nil {
		return command
	}
	theme := shellSyntaxTheme(dark)
	if theme == nil {
		return command
	}
	var rendered bytes.Buffer
	// A viewport renders only the physical lines currently on screen. Chroma
	// may represent an entire heredoc as one token, and the TTY formatter then
	// emits one opening SGR before its first line and one reset after its last.
	// Once the first line scrolls out of view, the terminal no longer receives
	// that opening SGR and the remaining heredoc appears unhighlighted. Format
	// each physical line independently so every viewport slice is a complete
	// ANSI stream with its own style prefixes and resets.
	for _, line := range chroma.SplitTokensIntoLines(iterator.Tokens()) {
		if err := formatters.TTY256.Format(&rendered, theme, chroma.Literator(line...)); err != nil {
			return command
		}
	}
	return strings.TrimRight(rendered.String(), "\r\n")
}

func shellSyntaxTheme(dark bool) *chroma.Style {
	shellSyntaxThemes.once.Do(func() {
		withoutBackground := func(name string) *chroma.Style {
			theme := styles.Get(name)
			if theme == nil {
				return nil
			}
			// Codex deliberately omits syntax-theme backgrounds so commands
			// remain native to the terminal surface. Chroma is already part of
			// ALT's Markdown stack; direct use avoids a home-grown tokenizer.
			result, err := theme.Builder().Transform(func(entry chroma.StyleEntry) chroma.StyleEntry {
				entry.Background = 0
				return entry
			}).Build()
			if err != nil {
				return nil
			}
			return result
		}
		shellSyntaxThemes.dark = withoutBackground("catppuccin-mocha")
		shellSyntaxThemes.light = withoutBackground("catppuccin-latte")
	})
	if dark {
		return shellSyntaxThemes.dark
	}
	return shellSyntaxThemes.light
}

// sanitizeToolOutput preserves only Select Graphic Rendition (SGR) color and
// emphasis sequences. Codex keeps command colors while rejecting cursor,
// screen, title, hyperlink, and clipboard control sequences; this implements
// the same boundary before output reaches ALT's viewport.
func sanitizeToolOutput(value string) string {
	var clean strings.Builder
	last := 0
	for _, location := range safeToolSGR.FindAllStringIndex(value, -1) {
		clean.WriteString(sanitizeTerminalText(value[last:location[0]]))
		clean.WriteString(value[location[0]:location[1]])
		last = location[1]
	}
	clean.WriteString(sanitizeTerminalText(value[last:]))
	return clean.String()
}

func visibleOutputLines(output string, expanded bool) []string {
	// Sanitization is role-aware in renderToolCell: command output may retain
	// safe SGR color, while patches and all other details are rendered plain.
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return nil
	}
	lines := strings.Split(output, "\n")
	if expanded || len(lines) <= 5 {
		return lines
	}
	return []string{
		lines[0], lines[1], lines[2],
		fmt.Sprintf("… +%d lines", len(lines)-4),
		lines[len(lines)-1],
	}
}

func decodeObject(value string) map[string]any {
	var result map[string]any
	if json.Unmarshal([]byte(value), &result) == nil {
		return result
	}
	return map[string]any{}
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func identifierValue(values map[string]any, key string) string {
	switch value := values[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func intValue(values map[string]any, key string) *int {
	value, ok := values[key]
	if !ok || value == nil {
		return nil
	}
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	result := int(number)
	return &result
}

func boolValue(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func compactJSON(value string) string {
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) != nil {
		return strings.TrimSpace(value)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return string(encoded)
}

func firstNonEmptyTUI(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type fileStat struct {
	added   int
	removed int
}

func patchFileStats(patch string) map[string]fileStat {
	result := make(map[string]fileStat)
	current := ""
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			path := cleanPatchPath(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")))
			if path != "/dev/null" {
				current = path
				if _, exists := result[current]; !exists {
					result[current] = fileStat{}
				}
			}
		case strings.HasPrefix(line, "--- "):
			path := cleanPatchPath(strings.TrimSpace(strings.TrimPrefix(line, "--- ")))
			if current == "" && path != "/dev/null" {
				current = path
				if _, exists := result[current]; !exists {
					result[current] = fileStat{}
				}
			}
		case current != "" && strings.HasPrefix(line, "+"):
			stat := result[current]
			stat.added++
			result[current] = stat
		case current != "" && strings.HasPrefix(line, "-"):
			stat := result[current]
			stat.removed++
			result[current] = stat
		}
	}
	return result
}

func cleanPatchPath(value string) string {
	value, _, _ = strings.Cut(value, "\t")
	value = strings.TrimPrefix(value, "a/")
	value = strings.TrimPrefix(value, "b/")
	return value
}

func sortedStatPaths(stats map[string]fileStat) []string {
	paths := make([]string, 0, len(stats))
	for path := range stats {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
