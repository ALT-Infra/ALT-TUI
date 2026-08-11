package tui

import (
	"fmt"
	"image/color"
	"math"
	"os"
	"strings"
	"time"
	"unicode"

	"altv1/internal/buildinfo"
	"altv1/internal/store"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type turnView struct {
	sessionID         string
	prompts           []string
	timeline          []string
	answer            string
	status            store.SessionStatus
	tokens            int
	compactions       int
	queuedDelegations int
	activeDelegations int
	startedAt         time.Time
	finishedAt        time.Time
	toolActivities    map[string]*toolActivity
	processTools      map[string]string
}

type statusSnapshot struct {
	team        string
	session     string
	state       string
	directory   string
	members     string
	tokens      string
	context     string
	queue       string
	permissions string
	research    string
}

type placedStatusSnapshot struct {
	afterTurn int
	snapshot  statusSnapshot
}

func (m *Model) beginTurn(prompt string) {
	m.turns = append(m.turns, turnView{
		prompts:   []string{prompt},
		status:    store.SessionRunning,
		startedAt: time.Now(),
	})
	m.currentTurn = len(m.turns) - 1
	m.starting = true
	m.status = "starting"
	m.touchTranscript(true)
}

func (m *Model) current() *turnView {
	if m.currentTurn < 0 || m.currentTurn >= len(m.turns) {
		return nil
	}
	return &m.turns[m.currentTurn]
}

func (m *Model) touchTranscript(follow bool) {
	wasBottom := m.viewport.AtBottom()
	m.transcriptRevision++
	m.renderTranscript()
	if follow && (wasBottom || m.forceFollow) {
		m.viewport.GotoBottom()
	}
	m.forceFollow = false
}

func (m *Model) renderTranscript() {
	width := max(1, m.viewport.Width())
	if m.renderedRevision == m.transcriptRevision && m.renderedWidth == width {
		return
	}
	var body strings.Builder
	m.selectionBlocks = nil
	lineCursor := 0
	write := func(value string) {
		body.WriteString(value)
		lineCursor += strings.Count(value, "\n")
	}
	writeSelectable := func(rendered, semantic string) {
		startLine := lineCursor
		write(rendered)
		block := newSelectionBlock(startLine, rendered, semantic)
		if len(block.lines) > 0 {
			m.selectionBlocks = append(m.selectionBlocks, block)
		}
	}
	header := m.renderSessionHeader(width)
	writeSelectable(header, semanticTextFromRendered(header))
	writeStatusCards := func(afterTurn int) {
		for _, placed := range m.statusCards {
			if placed.afterTurn != afterTurn {
				continue
			}
			write("\n")
			rendered := renderStatusSnapshot(placed.snapshot, width, m.darkBackground)
			writeSelectable(rendered, semanticTextFromRendered(rendered))
			write("\n")
		}
	}
	writeStatusCards(-1)
	if len(m.turns) == 0 {
		write("\n")
	} else {
		write("\n")
		for index := range m.turns {
			if index > 0 {
				write("\n")
			}
			turn := &m.turns[index]
			for _, prompt := range turn.prompts {
				rendered := renderUserPrompt(prompt, width, m.darkBackground, m.background)
				startLine := lineCursor
				write(rendered)
				m.selectionBlocks = append(
					m.selectionBlocks,
					newSelectionBlockIndented(startLine, rendered, strings.TrimRight(prompt, "\r\n"), 2),
				)
				write("\n")
			}
			if len(turn.timeline) > 0 {
				for _, line := range turn.timeline {
					var rendered string
					semantic := line
					if key, tool := strings.CutPrefix(line, toolTimelinePrefix); tool {
						rendered = renderToolActivity(
							turn.toolActivities[key], width, m.darkBackground, m.transcriptExpanded,
						)
						semantic = semanticTextFromRendered(rendered)
					} else {
						rendered = renderActivity(line, width, m.darkBackground)
					}
					writeSelectable(rendered, semantic)
					write("\n")
				}
			}
			if turn.status == store.SessionRunning {
				rendered := renderWorkingStatus(*turn, width, m.darkBackground)
				writeSelectable(rendered, semanticTextFromRendered(rendered))
				write("\n")
			}
			if strings.TrimSpace(turn.answer) != "" {
				write("\n")
				rendered := renderMarkdown(turn.answer, width, m.darkBackground)
				writeSelectable(rendered, turn.answer)
			}
			if !turn.finishedAt.IsZero() {
				rendered := renderTurnSummary(*turn, width, m.darkBackground)
				writeSelectable(rendered, semanticTextFromRendered(rendered))
				write("\n")
			}
			if turn.status == store.SessionFailed || turn.status == store.SessionCancelled {
				rendered := errorStyle(m.darkBackground).Render("■ " + strings.Title(string(turn.status)))
				writeSelectable(rendered, strings.Title(string(turn.status)))
				write("\n")
			}
			writeStatusCards(index)
		}
	}
	if len(m.pendingSteers) > 0 {
		write("\n")
		rendered := mutedStyle(m.darkBackground).Render(
			fmt.Sprintf("• Pending instructions (%d)", len(m.pendingSteers)),
		)
		writeSelectable(rendered, semanticTextFromRendered(rendered))
		for _, prompt := range m.pendingSteers {
			write("\n")
			rendered = mutedStyle(m.darkBackground).Render(
				"  ↳ " + ansi.Wrap(sanitizeTerminalText(prompt), width-4, " "),
			)
			writeSelectable(rendered, prompt)
		}
	}
	if len(m.queued) > 0 {
		write("\n")
		rendered := mutedStyle(m.darkBackground).Render(fmt.Sprintf("• Queued follow-up inputs (%d)", len(m.queued)))
		writeSelectable(rendered, semanticTextFromRendered(rendered))
		for _, prompt := range m.queued {
			write("\n")
			rendered = mutedStyle(m.darkBackground).Render("  ↳ " + ansi.Wrap(sanitizeTerminalText(prompt), width-4, " "))
			writeSelectable(rendered, prompt)
		}
	}
	m.renderedTranscript = strings.TrimRight(body.String(), "\n")
	m.viewport.SetContent(m.renderedTranscript)
	m.renderedRevision = m.transcriptRevision
	m.renderedWidth = width
}

func (m Model) captureStatus() statusSnapshot {
	team := "none"
	if m.profile != nil {
		team = fmt.Sprintf("%s@%d", m.profile.Profile.ID, m.profile.Profile.Revision)
	}
	session := "none"
	state := "idle"
	members := "0 active · 0 queued"
	tokens := "0"
	contextState := "no compaction"
	if current := m.current(); current != nil {
		reference := m.conversationID
		if reference == "" {
			reference = current.sessionID
		}
		if reference != "" {
			session = shortID(reference)
		}
		state = string(current.status)
		members = fmt.Sprintf("%d active · %d queued", current.activeDelegations, current.queuedDelegations)
		tokens = fmt.Sprintf("%d", current.tokens)
		if current.compactions > 0 {
			contextState = fmt.Sprintf("%d lossless compactions", current.compactions)
		}
	}
	permissions := "sandboxed · network isolated"
	if m.app.RuntimePolicy.DangerouslyBypassApprovalsAndSandbox {
		permissions = "dangerous host access"
	}
	researchProvider := m.app.RuntimePolicy.ResearchProvider
	if researchProvider == "" {
		researchProvider = "not selected"
	}
	return statusSnapshot{
		team: team, session: session, state: state, directory: m.workspace,
		members: members, tokens: tokens, context: contextState,
		queue: fmt.Sprintf("%d prompts", len(m.queued)), permissions: permissions,
		research: researchProvider,
	}
}

func renderStatusSnapshot(snapshot statusSnapshot, width int, dark bool) string {
	width = max(20, width)
	innerWidth := max(16, width-4)
	labelWidth := min(14, max(8, innerWidth/3))
	row := func(label, value string) string {
		label = label + ":"
		available := max(1, innerWidth-labelWidth-1)
		value = ansi.Truncate(sanitizeTerminalText(value), available, "…")
		content := "  " + fmt.Sprintf("%-*s", labelWidth, label) + " " + value
		content = ansi.Truncate(content, innerWidth, "")
		return "│ " + content + strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(content))) + " │"
	}
	border := strings.Repeat("─", innerWidth+2)
	lines := []string{
		lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render("/status"),
		"",
		"╭" + border + "╮",
		row("Team", snapshot.team),
		row("Session", snapshot.session+" · "+snapshot.state),
		row("Directory", snapshot.directory),
		row("Members", snapshot.members),
		row("Token usage", snapshot.tokens),
		row("Context", snapshot.context),
		row("Queue", snapshot.queue),
		row("Permissions", snapshot.permissions),
		row("Research", snapshot.research),
		"╰" + border + "╯",
	}
	for index := 2; index < len(lines); index++ {
		lines[index] = mutedStyle(dark).Render(lines[index])
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderSessionHeader(width int) string {
	team := "none"
	if m.profile != nil {
		team = fmt.Sprintf("%s@%d", m.profile.Profile.ID, m.profile.Profile.Revision)
	}
	directory := m.workspace
	if directory == "" {
		directory = "."
	}
	permissions := "sandboxed · network isolated"
	if m.app.RuntimePolicy.DangerouslyBypassApprovalsAndSandbox {
		permissions = "dangerous host access"
	}
	rows := []string{
		lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf(">_ ALT (v%s)", buildinfo.Version)),
		"",
		"team:        " + team + "   /profile to change",
		"directory:   " + directory,
		"permissions: " + permissions,
	}
	innerWidth := 0
	for _, row := range rows {
		innerWidth = max(innerWidth, ansi.StringWidth(ansi.Strip(row)))
	}
	innerWidth = min(max(34, innerWidth), max(10, width-2))
	border := strings.Repeat("─", innerWidth+2)
	var result strings.Builder
	result.WriteString("╭" + border + "╮\n")
	for _, row := range rows {
		row = ansi.Truncate(row, innerWidth, "…")
		padding := max(0, innerWidth-ansi.StringWidth(row))
		result.WriteString("│ " + row + strings.Repeat(" ", padding) + " │\n")
	}
	result.WriteString("╰" + border + "╯\n")
	return result.String()
}

func renderActivity(value string, width int, dark bool) string {
	value = sanitizeTerminalText(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	wrapped := ansi.Wrap(value, max(1, width-2), " ")
	lines := strings.Split(wrapped, "\n")
	for index := range lines {
		prefix := "  "
		if index == 0 {
			prefix = "• "
		}
		lines[index] = mutedStyle(dark).Render(prefix + lines[index])
	}
	return strings.Join(lines, "\n")
}

func renderWorkingStatus(turn turnView, width int, dark bool) string {
	label := "Working"
	switch {
	case turn.activeDelegations > 0:
		label = fmt.Sprintf("Working · %d member calls active", turn.activeDelegations)
	case turn.queuedDelegations > 0:
		label = fmt.Sprintf("Working · %d member calls queued", turn.queuedDelegations)
	}
	elapsed := time.Since(turn.startedAt)
	if turn.startedAt.IsZero() || elapsed < 0 {
		elapsed = 0
	}
	line := fmt.Sprintf("• %s (%s · esc to interrupt)", label, conciseDuration(elapsed))
	return mutedStyle(dark).Render(ansi.Truncate(line, width, "…"))
}

func renderTurnSummary(turn turnView, width int, dark bool) string {
	label := strings.Title(string(turn.status))
	if !turn.startedAt.IsZero() && !turn.finishedAt.Before(turn.startedAt) {
		label = "Worked for " + conciseDuration(turn.finishedAt.Sub(turn.startedAt))
	}
	if turn.tokens > 0 {
		label += fmt.Sprintf(" · %d tokens", turn.tokens)
	}
	if turn.compactions > 0 {
		label += fmt.Sprintf(" · %d context compactions", turn.compactions)
	}
	prefix := "─ " + label + " "
	fill := strings.Repeat("─", max(0, width-ansi.StringWidth(prefix)))
	return mutedStyle(dark).Render(prefix + fill)
}

func conciseDuration(value time.Duration) string {
	value = value.Round(time.Second)
	if value < time.Second {
		return "<1s"
	}
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value/time.Second))
	}
	return fmt.Sprintf("%dm%02ds", int(value/time.Minute), int(value%time.Minute/time.Second))
}

func renderUserPrompt(value string, width int, dark bool, background color.Color) string {
	value = sanitizeTerminalText(strings.TrimRight(value, "\r\n"))
	wrapped := ansi.Wrap(value, max(1, width-2), " ")
	lines := strings.Split(wrapped, "\n")
	for index := range lines {
		prefix := "  "
		if index == 0 {
			prefix = "› "
		}
		line := prefix + lines[index]
		line += strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
		lines[index] = userStyle(dark).Inherit(userSurfaceStyle(background)).Render(line)
	}
	blank := userSurfaceStyle(background).Render(strings.Repeat(" ", width))
	return blank + "\n" + strings.Join(lines, "\n") + "\n" + blank + "\n"
}

func renderMarkdown(value string, width int, dark bool) string {
	value = sanitizeTerminalText(value)
	style := "light"
	if dark {
		style = "dark"
	}
	styleOption := glamour.WithStandardStyle(style)
	if os.Getenv("GLAMOUR_STYLE") != "" {
		styleOption = glamour.WithEnvironmentConfig()
	}
	renderer, err := glamour.NewTermRenderer(
		styleOption,
		glamour.WithWordWrap(max(20, width)),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return ansi.Wrap(value, width, " ")
	}
	defer renderer.Close()
	rendered, err := renderer.Render(value)
	if err != nil {
		return ansi.Wrap(value, width, " ")
	}
	return strings.TrimRight(rendered, "\n") + "\n"
}

func sanitizeTerminalText(value string) string {
	value = ansi.Strip(value)
	var clean strings.Builder
	clean.Grow(len(value))
	for _, character := range value {
		switch {
		case character == '\n' || character == '\t':
			clean.WriteRune(character)
		case isDisallowedTerminalCharacter(character):
		default:
			clean.WriteRune(character)
		}
	}
	return clean.String()
}

func sanitizeTerminalTitle(value string) string {
	value = sanitizeTerminalText(value)
	var clean strings.Builder
	clean.Grow(len(value))
	pendingSpace := false
	for _, character := range value {
		if unicode.IsSpace(character) {
			pendingSpace = clean.Len() > 0
			continue
		}
		if pendingSpace {
			clean.WriteByte(' ')
			pendingSpace = false
		}
		clean.WriteRune(character)
	}
	return clean.String()
}

func isDisallowedTerminalCharacter(character rune) bool {
	if unicode.IsControl(character) {
		return true
	}
	return character == '\u00AD' ||
		character == '\u034F' ||
		character == '\u061C' ||
		character == '\u180E' ||
		(character >= '\u200B' && character <= '\u200F') ||
		(character >= '\u202A' && character <= '\u202E') ||
		(character >= '\u2060' && character <= '\u206F') ||
		(character >= '\uFE00' && character <= '\uFE0F') ||
		character == '\uFEFF' ||
		(character >= '\uFFF9' && character <= '\uFFFB') ||
		(character >= '\U0001BCA0' && character <= '\U0001BCA3') ||
		(character >= '\U000E0100' && character <= '\U000E01EF')
}

func userStyle(_ bool) lipgloss.Style {
	return lipgloss.NewStyle()
}

func userSurfaceStyle(background color.Color) lipgloss.Style {
	if background == nil {
		return lipgloss.NewStyle()
	}
	red, green, blue, _ := background.RGBA()
	r := uint8(red >> 8)
	g := uint8(green >> 8)
	b := uint8(blue >> 8)
	alpha := 0.12
	top := 255.0
	if !isDarkRGB(r, g, b) {
		alpha = 0.04
		top = 0
	}
	blend := func(component uint8) uint8 {
		return uint8(float64(component)*(1-alpha) + top*alpha + 0.5)
	}
	return lipgloss.NewStyle().Background(lipgloss.Color(fmt.Sprintf(
		"#%02X%02X%02X", blend(r), blend(g), blend(b),
	)))
}

func isDarkRGB(red, green, blue uint8) bool {
	// WCAG relative luminance is deterministic and avoids classifying saturated
	// colors by a naïve arithmetic average.
	linear := func(value uint8) float64 {
		component := float64(value) / 255
		if component <= 0.04045 {
			return component / 12.92
		}
		return math.Pow((component+0.055)/1.055, 2.4)
	}
	luminance := 0.2126*linear(red) + 0.7152*linear(green) + 0.0722*linear(blue)
	return luminance < 0.5
}

func errorStyle(_ bool) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
}

func warningStyle(_ bool) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
}
