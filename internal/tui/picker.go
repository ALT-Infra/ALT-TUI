package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"altv1/internal/application"
	"altv1/internal/provider"
	"altv1/internal/store"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const codexPickerMaxRows = 8

type pickerItem struct {
	kind        string
	reference   string
	title       string
	description string
	configured  bool
	session     *sessionPickerMeta
}

type sessionPickerMeta struct {
	updatedAt       time.Time
	workspace       string
	profileID       string
	profileRevision int
	status          store.SessionStatus
}

type pickerPageState struct {
	kind          string
	title         string
	generation    uint64
	loading       bool
	exhausted     bool
	sessionCursor *store.SessionCursor
	promptCursor  *store.PromptCursor
	seen          map[string]struct{}
	referenceTime time.Time
}

// codexListDelegate keeps selection surfaces textual: a cyan caret, default
// foreground for content, and dim supporting text. It intentionally avoids
// backgrounds, side borders, and bespoke RGB colors so the terminal theme
// remains authoritative.
type codexListDelegate struct {
	inline bool
}

// codexSessionDelegate supplies the geometry of Codex's comfortable session
// rows. The dedicated full-screen renderer owns their appearance; Bubbles owns
// only filtering, selection, and pagination state.
type codexSessionDelegate struct{}

func (codexSessionDelegate) Height() int                                  { return 2 }
func (codexSessionDelegate) Spacing() int                                 { return 1 }
func (codexSessionDelegate) Update(tea.Msg, *list.Model) tea.Cmd          { return nil }
func (codexSessionDelegate) Render(io.Writer, list.Model, int, list.Item) {}

func (d codexListDelegate) Height() int                         { return 1 }
func (d codexListDelegate) Spacing() int                        { return 0 }
func (d codexListDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d codexListDelegate) Render(writer io.Writer, model list.Model, index int, raw list.Item) {
	item, ok := raw.(pickerItem)
	if !ok {
		return
	}
	prefix := "  "
	titleStyle := lipgloss.NewStyle()
	if index == model.Index() {
		prefix = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("› ")
		titleStyle = titleStyle.Bold(true)
	}
	available := max(1, model.Width()-2)
	if d.inline {
		titleWidth := ansi.StringWidth(item.title)
		description := "  " + item.description
		if titleWidth+ansi.StringWidth(description) > available {
			description = "  " + ansi.Truncate(item.description, max(0, available-titleWidth-2), "…")
		}
		fmt.Fprint(writer, prefix+titleStyle.Render(item.title)+mutedStyle(true).Render(description))
		return
	}
	title := ansi.Truncate(item.title, available, "…")
	description := strings.TrimSpace(item.description)
	if description != "" {
		remaining := max(0, available-ansi.StringWidth(title)-2)
		if remaining > 6 {
			description = "  " + ansi.Truncate(description, remaining, "…")
		} else {
			description = ""
		}
	}
	fmt.Fprint(writer, prefix+titleStyle.Render(title)+mutedStyle(true).Render(description))
}

func (i pickerItem) Title() string       { return i.title }
func (i pickerItem) Description() string { return i.description }
func (i pickerItem) FilterValue() string {
	return i.title + " " + i.description + " " + i.reference
}

func (m *Model) openCommandPalette() {
	items := []list.Item{
		pickerItem{kind: "command", reference: "/new", title: "New session", description: "Clear the current view"},
		pickerItem{kind: "command", reference: "/resume", title: "Resume session", description: "Search durable conversation history"},
		pickerItem{kind: "command", reference: "/profile", title: "Choose Team Profile", description: "Search immutable profile revisions"},
		pickerItem{kind: "command", reference: "/team", title: "Team", description: "Create, edit, or inspect a Team in one graph window"},
		pickerItem{kind: "command", reference: "/auth", title: "Configure connection", description: "Store a gateway or research API key with hidden input"},
		pickerItem{kind: "command", reference: "/research", title: "Research mode", description: "Choose Exa or Linkup for web research"},
		pickerItem{kind: "command", reference: "/rename", title: "Rename session", description: "Give the active session a memorable title"},
		pickerItem{kind: "command", reference: "/copy", title: "Copy last response", description: "Copy the last answer as Markdown"},
		pickerItem{kind: "command", reference: "/status", title: "Session status", description: "Show current session configuration and usage"},
		pickerItem{kind: "command", reference: "/cancel", title: "Cancel run", description: "Stop the active orchestration"},
		pickerItem{kind: "command", reference: "/thinking", title: "Thinking graph", description: "Toggle the live floating execution graph"},
		pickerItem{kind: "command", reference: "/clear", title: "Clear conversation", description: "Clear the terminal and start a new conversation"},
		pickerItem{kind: "command", reference: "/exit", title: "Exit ALT", description: "Close ALT"},
	}
	picker := newCodexPicker(items, max(30, m.width-10), max(10, m.height-8), "Command palette")
	m.picker = &picker
	m.pickerPage = nil
	m.profilePicker = false
	m.input.Blur()
}

func (m *Model) openProfiles(items []store.ProfileSummary) {
	values := make([]list.Item, 0, len(items))
	for _, item := range items {
		description := fmt.Sprintf("%s@%d", item.ID, item.Revision)
		if m.profile != nil && m.profile.Profile.ID == item.ID && m.profile.Profile.Revision == item.Revision {
			description += " · current"
		}
		values = append(values, pickerItem{
			kind: "profile", reference: fmt.Sprintf("%s@%d", item.ID, item.Revision),
			title: item.Name, description: description,
		})
	}
	picker := newCodexPicker(values, max(1, m.width), codexPickerMaxRows, "Select Team Profile")
	picker.SetShowTitle(false)
	picker.SetShowPagination(false)
	m.picker = &picker
	m.pickerPage = nil
	m.profilePicker = true
	m.input.Blur()
	m.updateLayout()
}

func (m *Model) sizeProfilePicker() {
	if !m.profilePicker || m.picker == nil {
		return
	}
	rows := min(codexPickerMaxRows, max(1, m.height-9))
	m.picker.SetSize(max(1, m.width), rows)
}

func (m Model) profilePickerPageItems() ([]list.Item, int) {
	if m.picker == nil {
		return nil, 0
	}
	items := m.picker.VisibleItems()
	start, end := m.picker.Paginator.GetSliceBounds(len(items))
	return items[start:end], start
}

func (m Model) profilePickerHeight() int {
	items, _ := m.profilePickerPageItems()
	rows := max(1, len(items))
	return rows + 7
}

func (m Model) profilePickerView(width int) string {
	width = max(1, width)
	surface := userSurfaceStyle(m.background)
	background := surface.GetBackground()
	withBackground := func(style lipgloss.Style) lipgloss.Style {
		if background != nil {
			style = style.Background(background)
		}
		return style
	}
	plain := withBackground(lipgloss.NewStyle())
	strong := withBackground(lipgloss.NewStyle().Bold(true))
	muted := withBackground(mutedStyle(m.darkBackground))
	accent := withBackground(lipgloss.NewStyle().Foreground(lipgloss.Color("6")))

	line := func(parts ...string) string {
		value := strings.Join(parts, "")
		value = ansi.Truncate(value, width, "")
		padding := max(0, width-ansi.StringWidth(value))
		return value + plain.Render(strings.Repeat(" ", padding))
	}
	blank := line()
	lines := []string{
		blank,
		line(plain.Render("  "), strong.Render("Select Team Profile")),
	}
	if m.picker != nil && (m.picker.SettingFilter() || m.picker.FilterState() == list.FilterApplied) {
		lines = append(lines, line(plain.Render("  Search: "), accent.Render(m.picker.FilterInput.Value())))
	} else {
		lines = append(lines, line(plain.Render("  "), muted.Render("Choose the Team revision used for new turns.")))
	}
	lines = append(lines, blank)

	items, start := m.profilePickerPageItems()
	selected, _ := m.picker.SelectedItem().(pickerItem)
	if len(items) == 0 {
		lines = append(lines, line(plain.Render("  "), muted.Render("No matching Team Profiles.")))
	} else {
		nameColumn := 0
		for offset, raw := range items {
			item, ok := raw.(pickerItem)
			if !ok {
				continue
			}
			nameColumn = max(nameColumn, ansi.StringWidth(fmt.Sprintf("%d. %s", start+offset+1, item.title)))
		}
		nameColumn = min(nameColumn, max(1, (width-4)*7/10))
		for offset, raw := range items {
			item, ok := raw.(pickerItem)
			if !ok {
				continue
			}
			markerStyle := plain
			marker := "  "
			nameStyle := plain
			if item.reference == selected.reference {
				markerStyle = accent
				marker = "› "
				nameStyle = strong
			}
			name := fmt.Sprintf("%d. %s", start+offset+1, item.title)
			name = ansi.Truncate(name, nameColumn, "…")
			gap := strings.Repeat(" ", max(2, nameColumn-ansi.StringWidth(name)+2))
			lines = append(lines, line(
				markerStyle.Render(marker),
				nameStyle.Render(name),
				plain.Render(gap),
				muted.Render(item.description),
			))
		}
	}
	lines = append(lines,
		blank,
		line(plain.Render("  Press "), strong.Render("enter"), plain.Render(" to confirm or "), strong.Render("esc"), plain.Render(" to go back  "), muted.Render("/ to search")),
		blank,
	)
	return strings.Join(lines, "\n")
}

func (m *Model) openGateways(items []provider.GatewayDescriptor) {
	values := make([]list.Item, 0, len(items)+2)
	for _, item := range items {
		description := item.ID + " · store or replace API key"
		if item.Authentication == provider.AuthenticationDeviceOAuth {
			description = item.ID + " · sign in with account"
		}
		values = append(values, pickerItem{
			kind:        "gateway",
			reference:   item.ID,
			title:       item.Name,
			description: description,
		})
	}
	values = append(values, pickerItem{
		kind:        "gateway",
		reference:   "exa",
		title:       "Exa web research",
		description: "exa · store or replace research credential",
	})
	values = append(values, pickerItem{
		kind:        "gateway",
		reference:   "linkup",
		title:       "Linkup web research",
		description: "linkup · store or replace research credential",
	})
	picker := newCodexPicker(values, max(30, m.width-10), max(10, m.height-8), "Configure connection")
	m.picker = &picker
	m.pickerPage = nil
	m.profilePicker = false
	m.input.Blur()
}

func (m *Model) openResearchConnections(items []application.ResearchConnectionStatus) {
	values := make([]list.Item, 0, len(items))
	for _, item := range items {
		description := item.ID
		switch {
		case item.Selected:
			description += " · current"
		case item.Configured:
			description += " · ready"
		default:
			description += " · setup required"
		}
		values = append(values, pickerItem{
			kind: "research-provider", reference: item.ID,
			title: item.Name, description: description, configured: item.Configured,
		})
	}
	picker := newCodexPicker(values, max(30, m.width-10), max(10, m.height-8), "Research mode")
	m.picker = &picker
	m.pickerPage = nil
	m.profilePicker = false
	m.input.Blur()
}

func (m *Model) openPagedPicker(kind, title string) tea.Cmd {
	m.pickerGeneration++
	var picker list.Model
	if kind == "session" {
		picker = newCodexSessionPicker(max(1, m.width), max(1, m.height))
	} else {
		picker = newCodexPicker(nil, max(30, m.width-10), max(10, m.height-8), title+" · loading")
	}
	m.picker = &picker
	m.profilePicker = false
	m.pickerPage = &pickerPageState{
		kind:          kind,
		title:         title,
		generation:    m.pickerGeneration,
		loading:       true,
		seen:          make(map[string]struct{}),
		referenceTime: time.Now(),
	}
	m.input.Blur()
	return m.loadNextPickerPage()
}

func newCodexSessionPicker(width, height int) list.Model {
	picker := list.New(nil, codexSessionDelegate{}, max(1, width-4), max(1, height-8))
	picker.SetShowTitle(false)
	picker.SetShowStatusBar(false)
	picker.SetShowPagination(false)
	picker.SetShowHelp(false)
	picker.SetShowFilter(false)
	picker.DisableQuitKeybindings()
	picker.FilterInput.Prompt = ""
	return picker
}

func (m Model) isSessionPicker() bool {
	return m.picker != nil && m.pickerPage != nil && m.pickerPage.kind == "session"
}

func (m *Model) sizeSessionPicker() {
	if !m.isSessionPicker() {
		return
	}
	m.picker.SetSize(max(1, m.width-4), max(1, m.height-8))
}

func newCodexPicker(items []list.Item, width, height int, title string) list.Model {
	picker := list.New(items, codexListDelegate{}, width, height)
	picker.Title = title
	picker.SetShowStatusBar(false)
	picker.SetShowPagination(true)
	picker.SetShowHelp(false)
	picker.DisableQuitKeybindings()
	picker.FilterInput.Prompt = "Type to search: "
	styleCodexPicker(&picker)
	return picker
}

func styleCodexPicker(picker *list.Model) {
	if picker == nil {
		return
	}
	picker.Styles.Title = lipgloss.NewStyle().Bold(true)
	picker.Styles.TitleBar = lipgloss.NewStyle().Padding(0, 0, 1, 2)
	picker.Styles.HelpStyle = mutedStyle(true).Padding(1, 0, 0, 2)
	picker.Styles.PaginationStyle = mutedStyle(true).PaddingLeft(2)
	picker.Styles.NoItems = mutedStyle(true).PaddingLeft(2)
	picker.Styles.Filter.Focused.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	picker.Styles.Filter.Blurred.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
}

func (m *Model) stylePickers() {
	styleCodexPicker(m.picker)
	styleCodexPicker(m.slashPopup)
}

func (m *Model) loadNextPickerPage() tea.Cmd {
	if m.picker == nil || m.pickerPage == nil || m.pickerPage.exhausted {
		return nil
	}
	m.pickerPage.loading = true
	m.picker.Title = m.pickerPage.title + " · loading"
	limit := max(1, m.picker.Paginator.PerPage)
	switch m.pickerPage.kind {
	case "session":
		return loadSessionPageCmd(
			m.ctx,
			m.app.Store,
			m.pickerPage.generation,
			m.pickerPage.sessionCursor,
			limit,
		)
	case "history":
		return loadPromptPageCmd(
			m.ctx,
			m.app.Store,
			m.pickerPage.generation,
			m.pickerPage.promptCursor,
			limit,
		)
	default:
		return nil
	}
}

func (m *Model) maybeLoadNextPickerPage() tea.Cmd {
	if m.picker == nil || m.pickerPage == nil ||
		m.pickerPage.loading || m.pickerPage.exhausted {
		return nil
	}
	visible := m.picker.VisibleItems()
	if len(visible) == 0 ||
		(m.picker.Paginator.OnLastPage() && m.picker.Index() >= len(visible)-1) {
		return m.loadNextPickerPage()
	}
	return nil
}

func (m *Model) applySessionPage(msg sessionPageMsg) tea.Cmd {
	if m.picker == nil || m.pickerPage == nil ||
		m.pickerPage.kind != "session" ||
		m.pickerPage.generation != msg.generation {
		return nil
	}
	m.pickerPage.loading = false
	if msg.err != nil {
		m.pickerPage.exhausted = true
		m.picker.Title = m.pickerPage.title + " · load failed"
		m.status = "session history unavailable: " + msg.err.Error()
		return nil
	}
	values := append([]list.Item(nil), m.picker.Items()...)
	for _, item := range msg.page.Items {
		reference := item.ConversationID
		if reference == "" {
			reference = item.ID
		}
		if _, exists := m.pickerPage.seen[reference]; exists {
			continue
		}
		m.pickerPage.seen[reference] = struct{}{}
		values = append(values, pickerItem{
			kind: "session", reference: reference, title: item.Title,
			description: fmt.Sprintf(
				"%s %s@%d %s %s",
				item.Status, item.ProfileID, item.ProfileRevision, item.Workspace, shortID(reference),
			),
			session: &sessionPickerMeta{
				updatedAt: item.UpdatedAt, workspace: item.Workspace,
				profileID: item.ProfileID, profileRevision: item.ProfileRevision,
				status: item.Status,
			},
		})
	}
	m.pickerPage.sessionCursor = msg.page.Next
	m.pickerPage.exhausted = msg.page.Next == nil
	m.picker.Title = m.pickerPage.title
	return m.picker.SetItems(values)
}

func (m *Model) applyPromptPage(msg promptPageMsg) tea.Cmd {
	if m.picker == nil || m.pickerPage == nil ||
		m.pickerPage.kind != "history" ||
		m.pickerPage.generation != msg.generation {
		return nil
	}
	m.pickerPage.loading = false
	if msg.err != nil {
		m.pickerPage.exhausted = true
		m.picker.Title = m.pickerPage.title + " · load failed"
		m.status = "prompt history unavailable: " + msg.err.Error()
		return nil
	}
	values := append([]list.Item(nil), m.picker.Items()...)
	for _, item := range msg.page.Items {
		if _, exists := m.pickerPage.seen[item.SessionID]; exists {
			continue
		}
		m.pickerPage.seen[item.SessionID] = struct{}{}
		values = append(values, pickerItem{
			kind:        "history",
			reference:   item.Text,
			title:       firstLine(item.Text),
			description: "restore as editable draft",
		})
	}
	m.pickerPage.promptCursor = msg.page.Next
	m.pickerPage.exhausted = msg.page.Next == nil
	m.picker.Title = m.pickerPage.title
	return m.picker.SetItems(values)
}

func (m Model) activatePickerItem(item pickerItem) (tea.Model, tea.Cmd) {
	switch item.kind {
	case "command":
		if definition, ok := commandDefinitionFor(item.reference); ok && definition.needsInput {
			m.input.SetValue(item.reference + " ")
			m.input.CursorEnd()
			m.status = definition.description
			return m, nil
		}
		return m.handleCommand(item.reference)
	case "history":
		m.input.SetValue(item.reference)
		m.input.CursorEnd()
		m.status = "history restored as editable draft"
		return m, nil
	case "profile":
		return m, selectProfileCmd(m.ctx, m.app.Store, item.reference)
	case "gateway":
		command, err := authSetupCmd(item.reference)
		if err != nil {
			m.status = err.Error()
			return m, nil
		}
		m.input.Blur()
		m.status = "configuring " + item.reference + " credential"
		return m, command
	case "research-provider":
		if !item.configured {
			command, err := authSetupCmd(item.reference)
			if err != nil {
				m.status = err.Error()
				return m, nil
			}
			m.input.Blur()
			m.status = "configuring " + item.reference + " research credential"
			return m, command
		}
		m.status = "selecting " + item.reference + " research"
		return m, selectResearchProviderCmd(m.ctx, m.app, item.reference)
	case "session":
		if m.starting || (m.run != nil && m.app.Engine.Active(m.sessionID)) {
			m.status = "another session is active"
			return m, nil
		}
		m.resetSessionView()
		m.status = "opening session"
		return m, resolveConversationCmd(m.ctx, m.app, item.reference)
	default:
		return m, nil
	}
}

func (m Model) sessionPickerView(width, height int) string {
	width = max(1, width)
	height = max(1, height)
	plain := lipgloss.NewStyle()
	muted := mutedStyle(m.darkBackground)
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selected := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))

	fitLine := func(value string) string {
		value = ansi.Truncate(value, width, "")
		return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
	}
	leftRight := func(left, right string) string {
		gap := max(2, width-ansi.StringWidth(left)-ansi.StringWidth(right))
		if ansi.StringWidth(left)+gap+ansi.StringWidth(right) > width {
			left = ansi.Truncate(left, max(1, width-ansi.StringWidth(right)-2), "…")
			gap = max(2, width-ansi.StringWidth(left)-ansi.StringWidth(right))
		}
		return fitLine(left + strings.Repeat(" ", gap) + right)
	}

	query := ""
	if m.picker != nil {
		query = m.picker.FilterInput.Value()
	}
	search := muted.Render("Type to search")
	if query != "" {
		search = plain.Render("Search: " + sanitizeTerminalText(query))
	}
	lines := []string{
		fitLine(" " + header.Render("Resume a previous session")),
		fitLine(""),
		leftRight(" "+search, muted.Render("Sort: ")+plain.Render("[Updated]")),
		fitLine(""),
	}

	listHeight := max(1, height-8)
	listLines := make([]string, 0, listHeight)
	if m.picker == nil {
		listLines = append(listLines, fitLine("  "+muted.Render("No sessions yet")))
	} else {
		items := m.picker.VisibleItems()
		start, end := m.picker.Paginator.GetSliceBounds(len(items))
		if start > 0 {
			listLines = append(listLines, fitLine("  "+muted.Render("↑ more")))
		}
		for index := start; index < end && len(listLines)+2 <= listHeight; index++ {
			item, ok := items[index].(pickerItem)
			if !ok {
				continue
			}
			marker := "  "
			titleStyle := plain
			if index == m.picker.Index() {
				marker = selected.Render("❯ ")
				titleStyle = selected
			}
			title := ansi.Truncate(sanitizeTerminalTitle(item.title), max(1, width-4), "…")
			listLines = append(listLines, fitLine(" "+marker+titleStyle.Render(title)))
			listLines = append(listLines, fitLine(sessionPickerMetadata(item, width, m.pickerPage.referenceTime, muted)))
			if len(listLines) < listHeight {
				listLines = append(listLines, fitLine(""))
			}
		}
		if len(items) == 0 && !m.pickerPage.loading {
			listLines = append(listLines, fitLine("  "+muted.Render("No matching sessions")))
		} else if m.pickerPage.loading && len(items) == 0 {
			listLines = append(listLines, fitLine("  "+muted.Render("Loading sessions…")))
		} else if !m.pickerPage.exhausted && len(listLines) < listHeight {
			listLines = append(listLines, fitLine("  "+muted.Render("↓ more")))
		}
	}
	for len(listLines) < listHeight {
		listLines = append(listLines, fitLine(""))
	}
	lines = append(lines, listLines[:listHeight]...)

	position, total, complete := 0, 0, true
	if m.picker != nil {
		total = len(m.picker.VisibleItems())
		if total > 0 {
			position = m.picker.Index() + 1
		}
		complete = m.pickerPage == nil || m.pickerPage.exhausted
	}
	progress := fmt.Sprintf("%d / %d", position, total)
	if !complete {
		progress += "+"
	} else if total > 0 {
		progress += fmt.Sprintf(" · %d%%", position*100/total)
	}
	lines = append(lines, sessionPickerRule(width, progress, muted))
	escape := "start new"
	if query != "" {
		escape = "clear"
	}
	lines = append(lines,
		fitLine(" "+plain.Render("enter")+muted.Render(" resume   ")+plain.Render("esc")+muted.Render(" "+escape+"   ")+plain.Render("ctrl+c")+muted.Render(" quit")),
		fitLine(" "+plain.Render("↑/↓")+muted.Render(" browse   ")+plain.Render("pgup/pgdn")+muted.Render(" page   type to search")),
		fitLine(""),
	)
	return strings.Join(lines[:min(height, len(lines))], "\n")
}

func sessionPickerMetadata(item pickerItem, width int, reference time.Time, muted lipgloss.Style) string {
	if item.session == nil {
		return ""
	}
	when := relativeSessionTime(reference, item.session.updatedAt)
	workspace := strings.TrimSpace(item.session.workspace)
	if workspace == "" {
		workspace = "no cwd"
	}
	team := fmt.Sprintf("%s@%d · %s", item.session.profileID, item.session.profileRevision, item.session.status)
	// Three-column geometry: 3-cell inset, a fixed 12-cell timestamp, two
	// inter-column gaps, the two-cell cwd sigil, then the Team/status field.
	availableWorkspace := max(8, width-ansi.StringWidth(team)-21)
	workspace = ansi.Truncate(workspace, availableWorkspace, "…")
	workspace += strings.Repeat(" ", max(0, availableWorkspace-ansi.StringWidth(workspace)))
	return "   " + muted.Render(fmt.Sprintf("%-12s  ⌁ %s  %s", when, workspace, team))
}

func relativeSessionTime(reference, value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	delta := reference.Sub(value)
	if delta < 0 {
		delta = 0
	}
	switch {
	case delta < time.Minute:
		return fmt.Sprintf("%ds ago", int(delta/time.Second))
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta/time.Hour))
	case delta < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(delta/(24*time.Hour)))
	case delta < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(delta/(30*24*time.Hour)))
	default:
		return fmt.Sprintf("%dy ago", int(delta/(365*24*time.Hour)))
	}
}

func sessionPickerRule(width int, label string, muted lipgloss.Style) string {
	label = " " + label + " "
	if ansi.StringWidth(label)+1 >= width {
		return muted.Render(strings.Repeat("─", width))
	}
	return muted.Render(strings.Repeat("─", width-ansi.StringWidth(label)) + label)
}
