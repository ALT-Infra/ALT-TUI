package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"altv1/internal/application"
	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/store"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func (m Model) submit(queue bool) (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.input.Value())
	payload := composerPayload(m.input.Value(), m.composerAttachments)
	if payload.Input.Empty() {
		return m, nil
	}
	display := payloadDisplay(payload)

	if strings.HasPrefix(value, "/") && len(payload.Artifacts) == 0 {
		m.input.Reset()
		m.composerAttachments = nil
		m.slashPopup = nil
		m.history.record(value)
		m.updateLayout()
		return m.handleCommand(value)
	}
	if m.profile == nil {
		m.composerNotice = "Choose a Team with /profile, or create one with /team, before sending."
		m.status = "ready"
		m.updateLayout()
		return m, nil
	}
	m.composerNotice = ""
	m.input.Reset()
	m.composerAttachments = nil
	m.slashPopup = nil
	m.history.record(value)
	m.updateLayout()
	if m.starting {
		m.appendQueued(payload)
		m.status = fmt.Sprintf("queued while session starts · %d waiting", len(m.queued))
		m.touchTranscript(true)
		return m, nil
	}
	if m.active() {
		if queue {
			m.appendQueued(payload)
			m.status = fmt.Sprintf("queued · %d waiting", len(m.queued))
			m.touchTranscript(true)
			return m, nil
		}
		m.status = "steering the active leader"
		if current := m.current(); current != nil {
			current.prompts = append(current.prompts, display)
			m.optimisticSteers = append(m.optimisticSteers, display)
			m.pendingSteers = append(m.pendingSteers, display)
			m.touchTranscript(true)
		}
		return m, steerSessionCmd(m.ctx, m.app, m.sessionID, payload)
	}
	m.beginTurn(display)
	if m.sessionID != "" {
		return m, continueSessionCmd(m.ctx, m.app, m.sessionID, payload)
	}
	return m, startSessionCmd(m.ctx, m.app, m.profile, payload)
}

func (m Model) acceptSlashSelection() (tea.Model, tea.Cmd) {
	if m.slashPopup == nil {
		return m, nil
	}
	selected, ok := m.slashPopup.SelectedItem().(pickerItem)
	if !ok {
		return m, nil
	}
	definition, found := commandDefinitionFor(selected.reference)
	m.slashPopup = nil
	if found && definition.needsInput {
		m.input.SetValue(selected.reference + " ")
		m.input.CursorEnd()
		m.status = definition.description
		m.updateLayout()
		return m, nil
	}
	m.input.Reset()
	m.history.record(selected.reference)
	m.updateLayout()
	return m.handleCommand(selected.reference)
}

func (m *Model) editLastQueued() bool {
	if len(m.queued) == 0 {
		return false
	}
	lastIndex := len(m.queued) - 1
	m.ensureQueuedInputs()
	m.input.SetValue(m.queued[lastIndex])
	m.composerAttachments = append([]content.Artifact(nil), m.queuedInputs[lastIndex].Artifacts...)
	m.input.CursorEnd()
	m.queued = append([]string(nil), m.queued[:lastIndex]...)
	m.queuedInputs = append([]content.Payload(nil), m.queuedInputs[:lastIndex]...)
	m.status = "last queued prompt restored for editing"
	m.touchTranscript(true)
	m.updateLayout()
	return true
}

func (m *Model) restoreQueuedDraft() {
	if len(m.queued) == 0 {
		return
	}
	m.ensureQueuedInputs()
	queuedPayloads := append([]content.Payload(nil), m.queuedInputs...)
	if existing := strings.TrimSpace(m.input.Value()); existing != "" {
		queuedPayloads = append(queuedPayloads, composerPayload(m.input.Value(), m.composerAttachments))
	}
	draft, attachments := mergePayloadDrafts(queuedPayloads, len(m.queued))
	m.queued = nil
	m.queuedInputs = nil
	m.composerAttachments = attachments
	m.input.SetValue(draft)
	m.input.CursorEnd()
	m.input.Focus()
	m.status = "run ended; queued prompts restored for review"
	m.touchTranscript(true)
	m.updateLayout()
}

func (m *Model) syncSlashPopup() {
	value := m.input.Value()
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "\n") {
		m.slashPopup = nil
		return
	}
	items := commandMatches(value)
	if len(items) == 0 {
		m.slashPopup = nil
		return
	}
	previous := 0
	if m.slashPopup != nil {
		previous = m.slashPopup.Index()
	}
	m.slashPopup = newInlineCommandPopup(items, max(30, m.width-6))
	m.slashPopup.Select(min(previous, len(items)-1))
}

func (m *Model) openPromptHistory() tea.Cmd {
	return m.openPagedPicker("history", "Prompt history · / filters")
}

func (m *Model) updateLayout() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}
	inputWidth := max(1, width)
	m.input.SetWidth(inputWidth)
	if m.profilePicker && m.picker != nil {
		m.sizeProfilePicker()
		m.viewport.SetWidth(max(1, width))
		m.viewport.SetHeight(max(1, height-m.profilePickerHeight()-1))
		return
	}
	popupHeight := 0
	if m.slashPopup != nil {
		popupHeight = m.slashPopup.Height()
	}
	// Codex's composer reserves one quiet row above and below the textarea,
	// followed by one responsive footer row.
	const fixedChromeHeight = 4
	noticeHeight := 0
	if m.composerNotice != "" {
		noticeHeight = 1
	}
	available := max(2, height-fixedChromeHeight-popupHeight-noticeHeight)
	desiredInputHeight := wrappedLineCount(
		m.input.Value(),
		max(1, inputWidth-ansi.StringWidth(m.input.Prompt)),
	)
	// One transcript row remains visible; the rest of the live geometry can be
	// occupied by the draft when its wrapped content needs it.
	inputHeight := min(desiredInputHeight, available-1)
	m.input.SetHeight(max(1, inputHeight))
	viewportHeight := max(1, available-m.input.Height())
	m.viewport.SetWidth(max(1, width))
	m.viewport.SetHeight(viewportHeight)
}

func wrappedLineCount(value string, width int) int {
	if value == "" {
		return 1
	}
	return strings.Count(ansi.Wrap(value, max(1, width), ""), "\n") + 1
}

func (m Model) footer() string {
	var left string
	switch {
	case m.transcriptExpanded:
		left = "ctrl+t to close transcript"
	case !m.viewport.AtBottom():
		left = "pgdn/end for newer output"
	case m.starting && strings.TrimSpace(m.input.Value()) != "":
		left = "tab to queue message"
	case m.starting:
		left = "• Starting"
	case m.active() && strings.TrimSpace(m.input.Value()) != "":
		left = "tab to queue message"
	case m.active():
		left = "esc to interrupt"
	case strings.TrimSpace(m.input.Value()) != "":
		left = ""
	default:
		left = "? for shortcuts"
	}
	if status := m.visibleFooterStatus(); status != "" {
		left = status
	}
	if len(m.queued) > 0 && strings.TrimSpace(m.input.Value()) != "" {
		left = "tab to queue message"
	}
	right := m.footerContext()
	available := max(1, m.width-4)
	left = ansi.Truncate(left, available, "…")
	right = ansi.Truncate(right, available, "…")
	leftWidth := ansi.StringWidth(left)
	rightWidth := ansi.StringWidth(right)
	if leftWidth+rightWidth+2 > available {
		if leftWidth > 0 {
			right = ""
		} else {
			right = ansi.Truncate(right, available, "…")
		}
	}
	gap := max(0, available-ansi.StringWidth(left)-ansi.StringWidth(right))
	line := "  " + left + strings.Repeat(" ", gap) + right + "  "
	return mutedStyle(m.darkBackground).Render(line)
}

func (m Model) visibleFooterStatus() string {
	status := strings.TrimSpace(m.status)
	switch status {
	case "", "ready", "running", "starting", "completed", "cancelled":
		return ""
	}
	if strings.HasPrefix(status, "profile ") && strings.HasSuffix(status, " selected") {
		return status
	}
	for _, prefix := range []string{
		"error:", "failed", "no Team", "unknown ", "usage:", "select ",
		"session is", "another ", "gateway ", "prompt history ", "native window:",
	} {
		if strings.HasPrefix(status, prefix) {
			return status
		}
	}
	return ""
}

func (m Model) footerContext() string {
	team := "no team"
	if m.profile != nil {
		team = fmt.Sprintf("%s@%d", m.profile.Profile.ID, m.profile.Profile.Revision)
	}
	location := m.workspace
	if location == "" {
		location = "."
	}
	if base := filepath.Base(location); base != "." && base != string(filepath.Separator) {
		location = base
	}
	return team + " · " + location
}

func (m Model) shortcutOverlay() string {
	lines := []string{
		sectionStyle(m.darkBackground).Render("ALT shortcuts"),
		"",
		"Enter        submit / steer active leader",
		"Tab          queue while work is running",
		"Alt+Up       edit the most recent queued prompt",
		"Ctrl+J       insert newline",
		"Shift+Enter  insert newline when terminal supports it",
		"Up / Down    prompt history at editor boundaries",
		"Ctrl+R       search prompt history",
		"Ctrl+P       command palette",
		"PgUp/PgDn    scroll transcript",
		"Home / End   transcript top / bottom when draft is empty",
		"Ctrl+G       edit draft in $VISUAL or $EDITOR",
		"Ctrl+T       toggle complete tool transcript",
		"Esc          interrupt active orchestration",
		"Ctrl+C       interrupt, or quit when idle",
		"? / Esc      close this overlay",
	}
	width := min(72, max(36, m.width-8))
	return lipgloss.NewStyle().
		Width(width).
		Padding(1, 2).
		Render(strings.Join(lines, "\n"))
}

func (m Model) windowTitle() string {
	parts := []string{"ALT"}
	if m.profile != nil {
		parts = append(parts, m.profile.Profile.ID)
	}
	if m.sessionID != "" {
		reference := m.conversationID
		if reference == "" {
			reference = m.sessionID
		}
		parts = append(parts, shortID(reference))
	}
	if m.active() || m.starting {
		parts = append(parts, "running")
	}
	return ansi.Truncate(sanitizeTerminalTitle(strings.Join(parts, " · ")), 120, "…")
}

func (m Model) notificationCmd(item event.Event) tea.Cmd {
	if m.focused || !isTerminal(item.Kind) || len(m.queued) > 0 {
		return nil
	}
	message := "ALT is waiting for you"
	switch item.Kind {
	case event.FinalCompleted:
		data, _ := event.Decode[event.FinalCompletedData](item)
		preview := firstLine(data.Answer)
		if preview != "" {
			message = "ALT completed: " + preview
		} else {
			message = "ALT completed"
		}
	case event.SessionFailed:
		message = "ALT run failed"
	case event.SessionCancelled:
		message = "ALT run cancelled"
	}
	message = ansi.Truncate(sanitizeTerminalTitle(message), 160, "…")
	return tea.Raw(ansi.Notify(message))
}

func (m Model) lastAnswer() string {
	for index := len(m.turns) - 1; index >= 0; index-- {
		if strings.TrimSpace(m.turns[index].answer) != "" {
			return m.turns[index].answer
		}
	}
	return ""
}

func (m Model) active() bool {
	return m.sessionID != "" && m.app.Engine.Active(m.sessionID)
}

func (m Model) atInputStart() bool {
	info := m.input.LineInfo()
	return m.input.Line() == 0 && info.CharOffset == 0
}

func (m Model) atInputEnd() bool {
	lines := strings.Split(m.input.Value(), "\n")
	if len(lines) == 0 || m.input.Line() != len(lines)-1 {
		return false
	}
	return m.input.LineInfo().CharOffset >= len([]rune(lines[len(lines)-1]))
}

func firstLine(value string) string {
	line := strings.TrimSpace(strings.Split(value, "\n")[0])
	return ansi.Truncate(line, 72, "…")
}

func loadPromptHistorySnapshotCmd(ctx context.Context, ledger *store.Store) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := ledger.PromptSnapshot(ctx)
		return promptHistorySnapshotMsg{snapshot: snapshot, err: err}
	}
}

func loadPromptAtCmd(
	ctx context.Context,
	ledger *store.Store,
	lookup promptLookup,
) tea.Cmd {
	return func() tea.Msg {
		prompt, found, err := ledger.PromptAt(ctx, lookup.snapshot, lookup.offset)
		return promptLookupMsg{
			token: lookup.token, prompt: prompt, found: found, err: err,
		}
	}
}

func loadPromptPageCmd(
	ctx context.Context,
	ledger *store.Store,
	generation uint64,
	cursor *store.PromptCursor,
	limit int,
) tea.Cmd {
	return func() tea.Msg {
		page, err := ledger.ListPromptPage(ctx, cursor, limit)
		return promptPageMsg{generation: generation, page: page, err: err}
	}
}

func steerSessionCmd(
	ctx context.Context,
	app *application.Application,
	sessionID string,
	payload content.Payload,
) tea.Cmd {
	return func() tea.Msg {
		if err := app.Engine.SteerInput(ctx, sessionID, payload); err != nil {
			return steerRejectedMsg{prompt: payloadDisplay(payload), payload: payload, err: err}
		}
		return infoMsg("instruction delivered to the active leader")
	}
}

func cancelSessionCmd(ctx context.Context, app *application.Application, sessionID string) tea.Cmd {
	return func() tea.Msg {
		if err := app.Engine.Cancel(ctx, sessionID, "interrupted from TUI"); err != nil {
			return errorMsg{err}
		}
		return infoMsg("interruption requested")
	}
}
