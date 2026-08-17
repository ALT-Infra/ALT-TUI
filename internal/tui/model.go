package tui

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strconv"
	"strings"
	"time"

	"altv1/internal/application"
	"altv1/internal/content"
	"altv1/internal/event"
	"altv1/internal/nativegui"
	"altv1/internal/orchestrator"
	"altv1/internal/profile"
	"altv1/internal/store"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type Model struct {
	ctx                 context.Context
	app                 *application.Application
	profile             *profile.Document
	input               textarea.Model
	viewport            viewport.Model
	width               int
	height              int
	sessionID           string
	conversationID      string
	workspace           string
	starting            bool
	status              string
	composerNotice      string
	turns               []turnView
	currentTurn         int
	run                 *orchestrator.Run
	events              <-chan event.Event
	unsubscribe         func()
	picker              *list.Model
	pickerPage          *pickerPageState
	pickerGeneration    uint64
	profilePicker       bool
	teamGUI             *nativeProcess
	thinkingGUI         *nativeProcess
	slashPopup          *list.Model
	history             promptHistory
	queued              []string
	queuedInputs        []content.Payload
	composerAttachments []content.Artifact
	optimisticSteers    []string
	pendingSteers       []string
	shortcuts           bool
	transcriptExpanded  bool
	darkBackground      bool
	background          color.Color
	focused             bool
	selection           screenSelection
	selectionBlocks     []selectionBlock

	transcriptRevision int
	renderedRevision   int
	renderedWidth      int
	renderedTranscript string
	forceFollow        bool
	startupCmd         tea.Cmd
	statusCards        []placedStatusSnapshot
}

type sessionStartedMsg struct {
	run         *orchestrator.Run
	profile     *profile.Document
	history     []event.Event
	events      <-chan event.Event
	unsubscribe func()
}

type eventMsg struct {
	event event.Event
	ok    bool
}

type errorMsg struct {
	err error
}

type infoMsg string
type clockTickMsg time.Time
type promptHistorySnapshotMsg struct {
	snapshot store.PromptSnapshot
	err      error
}
type promptLookupMsg struct {
	token  uint64
	prompt string
	found  bool
	err    error
}
type steerRejectedMsg struct {
	prompt  string
	payload content.Payload
	err     error
}

type profileSelectedMsg struct {
	document *profile.Document
}

type profilesMsg struct{ items []store.ProfileSummary }
type researchConnectionsMsg struct {
	items []application.ResearchConnectionStatus
	err   error
}
type researchProviderSelectedMsg struct {
	provider string
	err      error
}
type sessionPageMsg struct {
	generation uint64
	page       store.SessionPage
	err        error
}
type promptPageMsg struct {
	generation uint64
	page       store.PromptPage
	err        error
}

type LaunchOptions struct {
	Workspace     string
	InitialPrompt string
	ResumePicker  bool
	ResumeSession string
}

func New(ctx context.Context, app *application.Application) Model {
	return NewWithOptions(ctx, app, LaunchOptions{})
}

func NewWithOptions(ctx context.Context, app *application.Application, options LaunchOptions) Model {
	input := textarea.New()
	input.Prompt = "› "
	input.Placeholder = "Ask ALT to do anything"
	input.ShowLineNumbers = false
	// The textarea dependency couples DynamicHeight to a legacy logical-line
	// guard when MaxContentHeight is zero. ALT instead keeps the draft
	// unbounded and measures only its visible viewport in updateLayout.
	input.DynamicHeight = false
	input.MaxHeight = 0
	input.MaxContentHeight = 0
	input.SetWidth(80)
	input.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j", "shift+enter", "alt+enter", "ctrl+enter"),
		key.WithHelp("ctrl+j", "newline"),
	)
	input.Focus()
	view := viewport.New()
	view.SoftWrap = true
	view.FillHeight = true
	workspace := strings.TrimSpace(options.Workspace)
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	model := Model{
		ctx:            ctx,
		app:            app,
		input:          input,
		viewport:       view,
		status:         "no Team selected · use /team or /profile",
		currentTurn:    -1,
		forceFollow:    true,
		darkBackground: true,
		focused:        true,
		workspace:      workspace,
	}
	initialPrompt := strings.TrimSpace(options.InitialPrompt)
	resumeSession := strings.TrimSpace(options.ResumeSession)
	if resumeSession != "" {
		model.starting = true
		model.status = "resuming"
		model.startupCmd = resolveConversationCmd(ctx, app, resumeSession)
		if initialPrompt != "" {
			model.appendQueued(content.TextPayload(initialPrompt))
		}
	} else if options.ResumePicker {
		model.startupCmd = model.openPagedPicker("session", "Resume a previous session")
		if initialPrompt != "" {
			model.appendQueued(content.TextPayload(initialPrompt))
		}
	} else if initialPrompt != "" {
		model.input.SetValue(initialPrompt)
		model.input.CursorEnd()
		model.composerNotice = "Choose a Team with /profile, or create one with /team, before sending."
	}
	model.updateLayout()
	model.touchTranscript(true)
	return model
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		loadPromptHistorySnapshotCmd(m.ctx, m.app.Store),
		func() tea.Msg { return tea.RequestBackgroundColor() },
		m.startupCmd,
	)
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case tea.BackgroundColorMsg:
		m.darkBackground = msg.IsDark()
		m.background = msg.Color
		m.styleComposerSurface()
		m.stylePickers()
		m.touchTranscript(false)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateLayout()
		if m.isSessionPicker() {
			m.sizeSessionPicker()
		} else if m.profilePicker {
			m.sizeProfilePicker()
		} else if m.picker != nil {
			m.picker.SetSize(max(30, msg.Width-10), max(10, msg.Height-8))
		}
		if m.slashPopup != nil {
			m.slashPopup.SetSize(max(30, msg.Width-6), min(8, max(3, len(m.slashPopup.Items())+1)))
		}
		m.touchTranscript(false)
	case tea.PasteMsg:
		if m.picker == nil && !m.shortcuts && m.attachImagePathFromPaste(msg.Content) {
			return m, nil
		}
	case tea.KeyPressMsg:
		if m.selection.active {
			switch msg.String() {
			case "ctrl+c":
				text := m.selectedText()
				m.selection.clear()
				if text != "" {
					m.status = "copied selection to clipboard"
					return m, tea.Batch(tea.SetClipboard(text), tea.SetPrimaryClipboard(text))
				}
			case "esc":
				m.selection.clear()
				return m, nil
			default:
				m.selection.clear()
			}
		}
		if m.picker != nil {
			if m.isSessionPicker() {
				if msg.Text != "" || (msg.String() == "backspace" && m.picker.FilterInput.Value() != "") {
					if !m.picker.SettingFilter() {
						m.picker.SetFilterState(list.Filtering)
					}
				}
				switch msg.String() {
				case "esc":
					if m.picker.FilterInput.Value() != "" || m.picker.SettingFilter() || m.picker.FilterState() == list.FilterApplied {
						m.picker.SetFilterText("")
						m.picker.SetFilterState(list.Unfiltered)
						return m, nil
					}
				case "enter":
					if selected, ok := m.picker.SelectedItem().(pickerItem); ok {
						m.picker = nil
						m.pickerPage = nil
						m.input.Focus()
						m.updateLayout()
						return m.activatePickerItem(selected)
					}
				case "up", "down", "pgup", "pgdown":
					if m.picker.SettingFilter() {
						m.picker.SetFilterState(list.FilterApplied)
					}
				}
			}
			switch msg.String() {
			case "ctrl+c", "ctrl+d":
				if m.unsubscribe != nil {
					m.unsubscribe()
				}
				m.stopNativeProcesses()
				return m, tea.Quit
			case "esc":
				if !m.picker.SettingFilter() && m.picker.FilterState() != list.FilterApplied {
					m.picker = nil
					m.pickerPage = nil
					m.profilePicker = false
					m.input.Focus()
					m.updateLayout()
					return m, nil
				}
			case "enter":
				if !m.picker.SettingFilter() {
					if selected, ok := m.picker.SelectedItem().(pickerItem); ok {
						m.picker = nil
						m.pickerPage = nil
						m.profilePicker = false
						m.input.Focus()
						m.updateLayout()
						return m.activatePickerItem(selected)
					}
				}
			}
			picker, cmd := m.picker.Update(message)
			m.picker = &picker
			m.updateLayout()
			return m, tea.Batch(cmd, m.maybeLoadNextPickerPage())
		}
		if m.shortcuts {
			switch msg.String() {
			case "?", "esc", "enter":
				m.shortcuts = false
			case "ctrl+c", "ctrl+d":
				m.stopNativeProcesses()
				return m, tea.Quit
			}
			return m, nil
		}
		if m.slashPopup != nil {
			switch msg.String() {
			case "esc":
				m.slashPopup = nil
				return m, nil
			case "up", "ctrl+p":
				m.slashPopup.CursorUp()
				return m, nil
			case "down", "ctrl+n":
				m.slashPopup.CursorDown()
				return m, nil
			case "tab":
				return m.acceptSlashSelection()
			case "enter":
				value := strings.TrimSpace(m.input.Value())
				if _, exact := commandDefinitionFor(value); exact {
					m.input.Reset()
					m.slashPopup = nil
					m.history.record(value)
					return m.handleCommand(value)
				}
				return m.acceptSlashSelection()
			}
		}
		switch msg.String() {
		case "ctrl+v", "ctrl+alt+v":
			m.status = "reading image from clipboard"
			return m, readClipboardImageCmd()
		case "ctrl+c":
			if m.active() {
				return m, cancelSessionCmd(m.ctx, m.app, m.sessionID)
			}
			if m.starting {
				m.stopNativeProcesses()
				return m, tea.Quit
			}
			m.stopNativeProcesses()
			return m, tea.Quit
		case "ctrl+d":
			if strings.TrimSpace(m.input.Value()) == "" {
				m.stopNativeProcesses()
				return m, tea.Quit
			}
		case "esc":
			if m.active() {
				return m, cancelSessionCmd(m.ctx, m.app, m.sessionID)
			}
			if m.starting {
				m.status = "session is starting; interrupt becomes available once it is durable"
				return m, nil
			}
			return m, nil
		case "enter":
			return m.submit(false)
		case "tab":
			return m.submit(true)
		case "alt+up", "shift+left":
			if m.editLastQueued() {
				return m, nil
			}
		case "ctrl+p":
			m.openCommandPalette()
			return m, nil
		case "ctrl+r":
			return m, m.openPromptHistory()
		case "ctrl+g":
			command, err := externalEditorCmd(m.input.Value())
			if err != nil {
				m.status = err.Error()
				return m, nil
			}
			m.input.Blur()
			m.status = "editing draft externally"
			return m, command
		case "ctrl+t":
			m.transcriptExpanded = !m.transcriptExpanded
			if m.transcriptExpanded {
				m.status = "full tool transcript visible"
			} else {
				m.status = "compact transcript visible"
			}
			m.touchTranscript(false)
			return m, nil
		case "pgup":
			m.viewport.PageUp()
			return m, nil
		case "pgdown":
			m.viewport.PageDown()
			return m, nil
		case "home":
			if strings.TrimSpace(m.input.Value()) == "" {
				m.viewport.GotoTop()
				return m, nil
			}
		case "end":
			if strings.TrimSpace(m.input.Value()) == "" {
				m.viewport.GotoBottom()
				return m, nil
			}
		case "?":
			if m.input.Value() == "" {
				m.shortcuts = true
				return m, nil
			}
		case "up":
			if m.atInputStart() {
				value, ok, lookup := m.history.older(m.input.Value())
				if lookup != nil {
					return m, loadPromptAtCmd(m.ctx, m.app.Store, *lookup)
				}
				if ok {
					m.input.SetValue(value)
					m.input.CursorEnd()
					m.syncSlashPopup()
					return m, nil
				}
			}
		case "down":
			if m.atInputEnd() {
				if value, ok := m.history.newer(); ok {
					m.input.SetValue(value)
					m.input.CursorEnd()
					m.syncSlashPopup()
					return m, nil
				}
			}
		}
	case tea.MouseWheelMsg:
		if m.picker != nil {
			picker, cmd := m.picker.Update(message)
			m.picker = &picker
			return m, tea.Batch(cmd, m.maybeLoadNextPickerPage())
		}
		view, cmd := m.viewport.Update(message)
		m.viewport = view
		return m, cmd
	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		if mouse.Button == tea.MouseLeft {
			m.selection.begin(screenPoint{X: mouse.X, Y: mouse.Y})
			return m, nil
		}
	case tea.MouseMotionMsg:
		mouse := msg.Mouse()
		if m.selection.dragging && mouse.Button == tea.MouseLeft {
			m.selection.extend(screenPoint{X: mouse.X, Y: mouse.Y})
			return m, nil
		}
	case tea.MouseReleaseMsg:
		if m.selection.dragging {
			mouse := msg.Mouse()
			m.selection.extend(screenPoint{X: mouse.X, Y: mouse.Y})
			m.selection.dragging = false
			text := m.selectedText()
			if text == "" {
				m.selection.clear()
				return m, nil
			}
			m.selection.clear()
			m.status = "copied selection to clipboard"
			return m, tea.Batch(tea.SetClipboard(text), tea.SetPrimaryClipboard(text))
		}
	case list.FilterMatchesMsg:
		if m.picker != nil {
			picker, cmd := m.picker.Update(message)
			m.picker = &picker
			m.updateLayout()
			return m, tea.Batch(cmd, m.maybeLoadNextPickerPage())
		}
	case clipboardImageMsg:
		if !msg.found {
			m.status = "clipboard does not contain an image"
			return m, nil
		}
		if err := m.attachImage(msg.data, "clipboard.png"); err != nil {
			m.status = "image paste: " + err.Error()
		}
		return m, nil
	case sessionStartedMsg:
		if msg.profile != nil {
			m.profile = msg.profile
		}
		for _, item := range msg.history {
			m.applyEvent(item)
		}
		if m.current() == nil || (m.current().sessionID != "" && m.current().sessionID != msg.run.SessionID) {
			m.turns = append(m.turns, turnView{
				sessionID: msg.run.SessionID,
				status:    store.SessionRunning,
			})
			m.currentTurn = len(m.turns) - 1
		}
		m.run = msg.run
		m.starting = false
		m.sessionID = msg.run.SessionID
		m.conversationID = msg.run.ConversationID
		m.workspace = msg.run.Workspace
		m.current().sessionID = msg.run.SessionID
		m.events = msg.events
		m.unsubscribe = msg.unsubscribe
		m.status = "running"
		m.touchTranscript(true)
		return m, tea.Batch(waitEventCmd(m.events), clockTickCmd())
	case clockTickMsg:
		if m.active() || m.starting {
			m.touchTranscript(m.viewport.AtBottom())
			return m, clockTickCmd()
		}
	case eventMsg:
		if !msg.ok {
			return m, nil
		}
		wasBottom := m.viewport.AtBottom()
		m.applyEvent(msg.event)
		m.touchTranscript(wasBottom)
		if m.events != nil && !isTerminal(msg.event.Kind) {
			return m, waitEventCmd(m.events)
		}
		if isTerminal(msg.event.Kind) && m.unsubscribe != nil {
			m.unsubscribe()
			m.unsubscribe = nil
		}
		notify := m.notificationCmd(msg.event)
		if msg.event.Kind == event.FinalCompleted && len(m.queued) > 0 {
			next, payload, _ := m.popQueued()
			m.beginTurn(next)
			return m, continueSessionCmd(m.ctx, m.app, m.sessionID, payload)
		}
		if (msg.event.Kind == event.SessionFailed ||
			msg.event.Kind == event.SessionCancelled) &&
			len(m.queued) > 0 {
			m.restoreQueuedDraft()
		}
		if notify != nil {
			return m, notify
		}
	case profileSelectedMsg:
		if (m.conversationID != "" || m.starting) &&
			(m.profile == nil ||
				msg.document.Profile.ID != m.profile.Profile.ID ||
				msg.document.Profile.Revision != m.profile.Profile.Revision) {
			m.status = "this session pins its Team Profile; use /new before switching"
			return m, nil
		}
		m.profile = msg.document
		m.composerNotice = ""
		m.status = fmt.Sprintf("profile %s@%d selected", msg.document.Profile.ID, msg.document.Profile.Revision)
		m.touchTranscript(true)
	case profilesMsg:
		m.openProfiles(msg.items)
		return m, nil
	case researchConnectionsMsg:
		if msg.err != nil {
			m.status = "research providers unavailable: " + msg.err.Error()
			return m, nil
		}
		m.openResearchConnections(msg.items)
		return m, nil
	case researchProviderSelectedMsg:
		m.input.Focus()
		if msg.err != nil {
			m.status = "research mode: " + msg.err.Error()
		} else {
			m.status = msg.provider + " research selected"
		}
		return m, nil
	case sessionPageMsg:
		cmd := m.applySessionPage(msg)
		return m, tea.Batch(cmd, m.maybeLoadNextPickerPage())
	case promptPageMsg:
		cmd := m.applyPromptPage(msg)
		return m, tea.Batch(cmd, m.maybeLoadNextPickerPage())
	case nativeStartedMsg:
		switch msg.process.mode {
		case nativegui.ModeThinking:
			m.thinkingGUI = msg.process
			m.status = "thinking graph opened in a separate window"
		default:
			m.teamGUI = msg.process
			m.status = "Team graph opened in a separate window"
		}
		return m, waitNativeCmd(msg.process)
	case nativeFinishedMsg:
		if m.thinkingGUI == msg.process {
			m.thinkingGUI = nil
		}
		if m.teamGUI == msg.process {
			m.teamGUI = nil
		}
		msg.process.closeUpdates()
		if msg.err != nil {
			m.status = "native window: " + msg.err.Error()
			return m, nil
		}
		if msg.published != nil {
			m.status = fmt.Sprintf(
				"published %s@%d",
				msg.published.ID,
				msg.published.Revision,
			)
			if m.conversationID == "" && !m.starting {
				return m, selectProfileCmd(
					m.ctx,
					m.app.Store,
					fmt.Sprintf("%s@%d", msg.published.ID, msg.published.Revision),
				)
			}
			m.status += " · current session remains pinned to its existing revision"
		} else if msg.process.mode == nativegui.ModeThinking {
			m.status = "thinking graph closed"
		} else {
			m.status = "Team graph closed"
		}
	case infoMsg:
		m.status = string(msg)
	case promptHistorySnapshotMsg:
		if msg.err != nil {
			m.status = "prompt history unavailable: " + msg.err.Error()
		} else {
			m.history.initialize(msg.snapshot)
		}
	case promptLookupMsg:
		if m.history.pending == nil || m.history.pending.token != msg.token {
			break
		}
		if msg.err != nil {
			m.history.pending = nil
			m.status = "prompt history unavailable: " + msg.err.Error()
			break
		}
		if value, ok := m.history.resolve(msg.token, msg.prompt, msg.found); ok {
			m.input.SetValue(value)
			m.input.CursorEnd()
			m.syncSlashPopup()
		}
	case steerRejectedMsg:
		m.optimisticSteers = removeFirst(m.optimisticSteers, msg.prompt)
		m.pendingSteers = removeFirst(m.pendingSteers, msg.prompt)
		m.prependQueued(msg.prompt, msg.payload)
		m.status = "Current leader could not be steered; message queued for the next turn: " + msg.err.Error()
		m.touchTranscript(true)
	case errorMsg:
		m.starting = false
		m.status = "error: " + msg.err.Error()
		if current := m.current(); current != nil {
			current.timeline = append(current.timeline, "× "+msg.err.Error())
			current.status = store.SessionFailed
			m.touchTranscript(true)
		}
	case editorFinishedMsg:
		m.input.Focus()
		if msg.err != nil {
			m.status = "editor: " + msg.err.Error()
		} else {
			m.input.SetValue(msg.text)
			m.input.CursorEnd()
			m.status = "draft restored from editor"
		}
	case authFinishedMsg:
		m.input.Focus()
		if msg.err != nil {
			m.status = "connection setup: " + msg.err.Error()
		} else {
			if msg.connection == "exa" || msg.connection == "linkup" {
				m.status = msg.connection + " configured · selecting research mode"
				return m, selectResearchProviderCmd(m.ctx, m.app, msg.connection)
			}
			m.status = "gateway credential configured"
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(message)
	if _, keyMessage := message.(tea.KeyPressMsg); keyMessage {
		m.history.resetNavigation()
	}
	m.syncSlashPopup()
	m.updateLayout()
	return m, cmd
}

func (m Model) View() tea.View {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}
	if m.isSessionPicker() {
		content := m.sessionPickerView(width, height)
		view := tea.NewView(content)
		view.AltScreen = true
		view.ReportFocus = true
		view.MouseMode = tea.MouseModeCellMotion
		view.WindowTitle = m.windowTitle()
		return view
	}

	transcript := m.viewport.View()
	var pieces []string
	pieces = append(pieces, transcript)
	if m.profilePicker && m.picker != nil {
		pieces = append(pieces, m.profilePickerView(width), m.footer())
	} else {
		composer := userSurfaceStyle(m.background).
			Width(width).
			Padding(1, 0).
			Render(m.input.View())
		if m.slashPopup != nil {
			pieces = append(pieces, m.slashPopup.View())
		}
		if m.composerNotice != "" {
			pieces = append(pieces,
				warningStyle(m.darkBackground).PaddingLeft(2).Render("⚠ "+m.composerNotice),
			)
		}
		pieces = append(pieces, composer, m.footer())
	}
	content := lipgloss.JoinVertical(lipgloss.Left, pieces...)
	if m.picker != nil && !m.profilePicker {
		pickerFooter := mutedStyle(m.darkBackground).Render(
			"  enter to confirm     esc to go back     / to search",
		)
		modal := userSurfaceStyle(m.background).
			Width(max(30, width-10)).
			Padding(1, 2).
			Render(m.picker.View() + "\n\n" + pickerFooter)
		content = lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
	}
	if m.shortcuts {
		content = lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center,
			userSurfaceStyle(m.background).Render(m.shortcutOverlay()))
	}
	if m.selection.active {
		content = m.highlightSelection(content)
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.ReportFocus = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = m.windowTitle()
	return view
}

func (m *Model) styleComposerSurface() {
	if m.background == nil {
		return
	}
	background := userSurfaceStyle(m.background).GetBackground()
	styles := textarea.DefaultStyles(m.darkBackground)
	paint := func(state textarea.StyleState) textarea.StyleState {
		state.Base = state.Base.Background(background)
		state.Text = state.Text.Background(background)
		state.LineNumber = state.LineNumber.Background(background)
		state.CursorLineNumber = state.CursorLineNumber.Background(background)
		state.CursorLine = state.CursorLine.Background(background)
		state.EndOfBuffer = state.EndOfBuffer.Background(background)
		state.Placeholder = state.Placeholder.Background(background)
		state.Prompt = state.Prompt.Background(background)
		return state
	}
	styles.Focused = paint(styles.Focused)
	styles.Blurred = paint(styles.Blurred)
	m.input.SetStyles(styles)
}

func (m Model) handleCommand(command string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(command)
	switch fields[0] {
	case "/quit", "/exit":
		if len(fields) != 1 {
			m.status = "usage: " + fields[0]
			return m, nil
		}
		if m.unsubscribe != nil {
			m.unsubscribe()
		}
		m.stopNativeProcesses()
		return m, tea.Quit
	case "/new", "/clear":
		if m.active() || m.starting {
			m.status = "cancel the active session before starting a new one"
			return m, nil
		}
		m.resetSessionView()
		m.status = "ready"
		m.touchTranscript(true)
		prompt := strings.TrimSpace(strings.TrimPrefix(command, fields[0]))
		if prompt != "" {
			m.input.SetValue(prompt)
			m.input.CursorEnd()
			m.updateLayout()
			return m.submit(false)
		}
		return m, nil
	case "/cancel":
		if len(fields) != 1 {
			m.status = "usage: /cancel"
			return m, nil
		}
		if m.sessionID == "" {
			m.status = "no session selected"
			return m, nil
		}
		return m, cancelSessionCmd(m.ctx, m.app, m.sessionID)
	case "/team":
		if len(fields) > 2 {
			m.status = "usage: /team [id[@revision]]"
			return m, nil
		}
		if m.teamGUI != nil {
			m.status = "a Team window is already open"
			return m, nil
		}
		launch := nativegui.Launch{Mode: nativegui.ModeTeam}
		if len(fields) == 2 {
			id, revision, err := parseNativeProfileReference(fields[1])
			if err != nil {
				m.status = err.Error()
				return m, nil
			}
			launch.ProfileID, launch.Revision = id, revision
		} else if m.profile != nil {
			launch = selectedTeamInspectorLaunch(m.profile)
		}
		m.status = "opening Team graph"
		return m, launchNativeCmd(m.ctx, m.app.DataDir, m.app.RuntimePolicy.DangerouslyBypassApprovalsAndSandbox, launch)
	case "/auth":
		if len(fields) != 1 {
			m.status = "usage: /auth"
			return m, nil
		}
		m.openGateways(m.app.Providers.Descriptors())
		m.status = "choose a connection"
		return m, nil
	case "/research":
		if len(fields) != 1 {
			m.status = "usage: /research"
			return m, nil
		}
		m.status = "loading research providers"
		return m, loadResearchConnectionsCmd(m.ctx, m.app)
	case "/profile":
		if len(fields) == 1 {
			return m, loadProfilesCmd(m.ctx, m.app.Store)
		}
		if len(fields) != 2 {
			m.status = "usage: /profile id[@revision]"
			return m, nil
		}
		return m, selectProfileCmd(m.ctx, m.app.Store, fields[1])
	case "/resume":
		if len(fields) == 1 {
			return m, m.openPagedPicker("session", "Resume a previous session")
		}
		if len(fields) != 2 {
			m.status = "usage: /resume [session-id]"
			return m, nil
		}
		if m.starting || (m.run != nil && m.app.Engine.Active(m.sessionID)) {
			m.status = "another session is active"
			return m, nil
		}
		m.resetSessionView()
		m.status = "resuming"
		return m, resolveConversationCmd(m.ctx, m.app, fields[1])
	case "/rename":
		if m.sessionID == "" {
			m.status = "start or resume a session before renaming it"
			return m, nil
		}
		if len(fields) < 2 {
			m.input.SetValue("/rename ")
			m.input.CursorEnd()
			m.input.Focus()
			m.status = "enter a new session name"
			return m, nil
		}
		return m, renameSessionCmd(
			m.ctx, m.app.Store, m.sessionID,
			strings.TrimSpace(strings.TrimPrefix(command, "/rename")),
		)
	case "/copy":
		if len(fields) != 1 {
			m.status = "usage: /copy"
			return m, nil
		}
		answer := m.lastAnswer()
		if strings.TrimSpace(answer) == "" {
			m.status = "no answer to copy"
			return m, nil
		}
		m.status = "copied last answer"
		return m, tea.SetClipboard(answer)
	case "/status":
		if len(fields) != 1 {
			m.status = "usage: /status"
			return m, nil
		}
		snapshot := m.captureStatus()
		m.statusCards = append(m.statusCards, placedStatusSnapshot{
			afterTurn: len(m.turns) - 1,
			snapshot:  snapshot,
		})
		m.status = "status"
		m.touchTranscript(true)
		return m, nil
	case "/thinking":
		if len(fields) != 1 {
			m.status = "usage: /thinking"
			return m, nil
		}
		if m.thinkingGUI != nil {
			process := m.thinkingGUI
			m.status = "closing thinking graph"
			return m, stopNativeCmd(process)
		}
		if m.sessionID == "" {
			m.status = "start or open a session before using /thinking"
			return m, nil
		}
		m.status = "opening thinking graph"
		return m, launchNativeCmd(m.ctx, m.app.DataDir, m.app.RuntimePolicy.DangerouslyBypassApprovalsAndSandbox, nativegui.Launch{
			Mode: nativegui.ModeThinking, SessionID: m.sessionID,
		})
	default:
		m.status = "unknown command " + fields[0]
		return m, nil
	}
}

func (m *Model) applyEvent(item event.Event) {
	current := m.current()
	if current == nil || (current.sessionID != "" && current.sessionID != item.SessionID) {
		m.turns = append(m.turns, turnView{sessionID: item.SessionID, status: store.SessionRunning})
		m.currentTurn = len(m.turns) - 1
		current = m.current()
	}
	switch item.Kind {
	case event.SessionCreated:
		data, _ := event.Decode[event.SessionCreatedData](item)
		if current.startedAt.IsZero() {
			current.startedAt = item.At
		}
		if len(current.prompts) == 0 {
			current.prompts = append(current.prompts, data.Task)
		}
	case event.UserInstruction:
		data, _ := event.Decode[event.UserInstructionData](item)
		if len(m.optimisticSteers) > 0 && m.optimisticSteers[0] == data.Text {
			m.optimisticSteers = append([]string(nil), m.optimisticSteers[1:]...)
		} else {
			current.prompts = append(current.prompts, data.Text)
		}
	case event.AgentTurnStarted:
		data, _ := event.Decode[event.AgentTurnData](item)
		for _, kind := range data.SignalKinds {
			if kind == string(event.UserInstruction) {
				m.pendingSteers = nil
				break
			}
		}
	case event.LeadershipTransferred:
		data, _ := event.Decode[event.LeadershipTransferredData](item)
		label := "Entered through " + data.ToAgentID
		if data.FromAgentID != "" {
			label = "Leadership " + data.FromAgentID + " → " + data.ToAgentID
		}
		current.timeline = append(current.timeline, activityDetail(label, data.Reason))
	case event.AgentDecision:
		data, _ := event.Decode[event.AgentDecisionData](item)
		current.timeline = append(current.timeline, activityDetail("Coordinating", data.Assessment))
	case event.DelegationCreated:
		data, _ := event.Decode[event.DelegationSpec](item)
		current.queuedDelegations++
		current.timeline = append(current.timeline, activityDetail("Called specialist "+data.SpecialistID, data.Objective))
	case event.PeerTurnCreated:
		data, _ := event.Decode[event.PeerTurnSpec](item)
		current.queuedDelegations++
		current.timeline = append(current.timeline, activityDetail("Consulted peer "+data.PeerID, data.Objective))
	case event.DelegationStarted:
		data, _ := event.Decode[event.DelegationStartedData](item)
		if current.queuedDelegations > 0 {
			current.queuedDelegations--
		}
		current.activeDelegations++
		current.timeline = append(current.timeline, item.Actor+" working (attempt "+strconv.Itoa(data.Attempt)+")")
	case event.ToolCalled:
		data, _ := event.Decode[event.ToolCallData](item)
		current.recordToolCall(item.Actor, data)
	case event.ToolCompleted:
		data, _ := event.Decode[event.ToolCompletedData](item)
		current.recordToolCompletion(item.Actor, data)
	case event.DelegationCompleted:
		finishDelegation(current)
		current.timeline = append(current.timeline, item.Actor+" returned")
	case event.DelegationFailed:
		data, _ := event.Decode[event.DelegationFailedData](item)
		finishDelegation(current)
		current.timeline = append(current.timeline, activityDetail(item.Actor+" failed", data.Error))
	case event.DelegationCancelled:
		finishDelegation(current)
		current.timeline = append(current.timeline, "Delegation cancelled")
	case event.PeerTurnStarted:
		data, _ := event.Decode[event.PeerTurnStartedData](item)
		if current.queuedDelegations > 0 {
			current.queuedDelegations--
		}
		current.activeDelegations++
		current.timeline = append(current.timeline, item.Actor+" consulting (attempt "+strconv.Itoa(data.Attempt)+")")
	case event.PeerTurnCompleted:
		finishDelegation(current)
		current.timeline = append(current.timeline, item.Actor+" returned consultation")
	case event.PeerTurnFailed:
		data, _ := event.Decode[event.PeerTurnFailedData](item)
		finishDelegation(current)
		current.timeline = append(current.timeline, activityDetail(item.Actor+" consultation failed", data.Error))
	case event.PeerTurnCancelled:
		finishDelegation(current)
		current.timeline = append(current.timeline, "Peer consultation cancelled")
	case event.FinalStarted:
		current.timeline = append(current.timeline, item.Actor+" answering")
	case event.FinalTextDelta:
		data, _ := event.Decode[event.TextDeltaData](item)
		current.answer += data.Text
	case event.FinalCompleted:
		data, _ := event.Decode[event.FinalCompletedData](item)
		current.answer = data.Answer
		current.status = store.SessionCompleted
		current.finishedAt = item.At
		m.status = "completed"
	case event.ModelUsage:
		data, _ := event.Decode[event.ModelUsageData](item)
		current.tokens += data.TotalTokens
	case event.ContextViewCommitted:
		data, _ := event.Decode[event.ContextViewCommittedData](item)
		if data.Compacted {
			current.compactions++
		}
	case event.ContextAgentCompacted:
		data, _ := event.Decode[event.ContextAgentCompactedData](item)
		current.compactions++
		current.timeline = append(current.timeline, fmt.Sprintf(
			"Context compacted for %s · %d → %d messages · exact transcript retained",
			data.Scope, data.MessagesBefore, data.MessagesAfter,
		))
	case event.SessionFailed:
		data, _ := event.Decode[event.FailureData](item)
		current.status = store.SessionFailed
		current.finishedAt = item.At
		m.status = "failed: " + data.Error
	case event.SessionCancelled:
		current.status = store.SessionCancelled
		current.finishedAt = item.At
		m.status = "cancelled"
	}
	if m.thinkingGUI != nil {
		m.thinkingGUI.pushEvent(item)
	}
}

func activityDetail(label, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return label
	}
	return label + "\n  └ " + detail
}

func finishDelegation(turn *turnView) {
	if turn.activeDelegations > 0 {
		turn.activeDelegations--
		return
	}
	if turn.queuedDelegations > 0 {
		turn.queuedDelegations--
	}
}

func removeFirst(values []string, target string) []string {
	for index, value := range values {
		if value == target {
			return append(values[:index:index], values[index+1:]...)
		}
	}
	return values
}

func (m *Model) resetSessionView() {
	if m.unsubscribe != nil {
		m.unsubscribe()
	}
	m.sessionID = ""
	m.conversationID = ""
	m.starting = false
	m.run = nil
	m.events = nil
	m.unsubscribe = nil
	m.turns = nil
	m.currentTurn = -1
	m.queued = nil
	m.queuedInputs = nil
	m.composerAttachments = nil
	m.optimisticSteers = nil
	m.pendingSteers = nil
	m.transcriptExpanded = false
	m.statusCards = nil
	m.renderedRevision = -1
}

func startSessionCmd(ctx context.Context, app *application.Application, document *profile.Document, payload content.Payload) tea.Cmd {
	return func() tea.Msg {
		run, err := app.Engine.StartInput(ctx, document, payload)
		if err != nil {
			return errorMsg{err}
		}
		events, unsubscribe, err := app.Store.Subscribe(ctx, run.SessionID, 0)
		if err != nil {
			return errorMsg{err}
		}
		return sessionStartedMsg{
			run: run, profile: document, events: events, unsubscribe: unsubscribe,
		}
	}
}

func continueSessionCmd(
	ctx context.Context,
	app *application.Application,
	previousSessionID string,
	payload content.Payload,
) tea.Cmd {
	return func() tea.Msg {
		run, err := app.Engine.ContinueInput(ctx, previousSessionID, payload)
		if err != nil {
			return errorMsg{err}
		}
		events, unsubscribe, err := app.Store.Subscribe(ctx, run.SessionID, 0)
		if err != nil {
			return errorMsg{err}
		}
		return sessionStartedMsg{run: run, events: events, unsubscribe: unsubscribe}
	}
}

func openSessionCmd(
	ctx context.Context,
	app *application.Application,
	sessionID string,
) tea.Cmd {
	return func() tea.Msg {
		turns, err := app.Store.ConversationSessions(ctx, sessionID)
		if err != nil {
			return errorMsg{err}
		}
		if len(turns) == 0 {
			return errorMsg{fmt.Errorf("session not found")}
		}
		latest := turns[len(turns)-1]
		document, err := app.Store.Profile(ctx, latest.ProfileID, latest.ProfileRevision)
		if err != nil {
			return errorMsg{err}
		}
		history := make([]event.Event, 0)
		for _, turn := range turns[:len(turns)-1] {
			items, err := app.Store.Events(ctx, turn.ID, 0)
			if err != nil {
				return errorMsg{err}
			}
			history = append(history, items...)
		}
		var run *orchestrator.Run
		if latest.Status == store.SessionRunning {
			run, err = app.Engine.Resume(ctx, latest.ID)
			if err != nil {
				return errorMsg{err}
			}
		} else {
			run = &orchestrator.Run{
				SessionID: latest.ID, ConversationID: latest.ConversationID,
				Workspace: latest.Workspace,
			}
		}
		events, unsubscribe, err := app.Store.Subscribe(ctx, latest.ID, 0)
		if err != nil {
			return errorMsg{err}
		}
		return sessionStartedMsg{
			run: run, profile: document, history: history,
			events: events, unsubscribe: unsubscribe,
		}
	}
}

func waitEventCmd(events <-chan event.Event) tea.Cmd {
	return func() tea.Msg {
		item, ok := <-events
		return eventMsg{event: item, ok: ok}
	}
}

func clockTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(value time.Time) tea.Msg {
		return clockTickMsg(value)
	})
}

func loadProfilesCmd(ctx context.Context, ledger *store.Store) tea.Cmd {
	return func() tea.Msg {
		items, err := ledger.ListProfiles(ctx)
		if err != nil {
			return errorMsg{err}
		}
		return profilesMsg{items: items}
	}
}

func loadResearchConnectionsCmd(ctx context.Context, app *application.Application) tea.Cmd {
	return func() tea.Msg {
		items, err := app.ResearchConnections(ctx)
		return researchConnectionsMsg{items: items, err: err}
	}
}

func selectResearchProviderCmd(ctx context.Context, app *application.Application, provider string) tea.Cmd {
	return func() tea.Msg {
		err := app.SelectResearchProvider(ctx, provider)
		return researchProviderSelectedMsg{provider: provider, err: err}
	}
}

func selectProfileCmd(ctx context.Context, ledger *store.Store, reference string) tea.Cmd {
	return func() tea.Msg {
		id := reference
		value := 0
		if profileID, revision, ok := strings.Cut(reference, "@"); ok {
			id = profileID
			parsed, err := strconv.Atoi(revision)
			if err != nil || parsed < 1 {
				return errorMsg{fmt.Errorf("invalid profile revision")}
			}
			value = parsed
		}
		document, err := ledger.Profile(ctx, id, value)
		if err != nil {
			return errorMsg{err}
		}
		return profileSelectedMsg{document: document}
	}
}

func loadSessionPageCmd(
	ctx context.Context,
	ledger *store.Store,
	generation uint64,
	cursor *store.SessionCursor,
	limit int,
) tea.Cmd {
	return func() tea.Msg {
		page, err := ledger.ListSessionPage(ctx, cursor, limit)
		return sessionPageMsg{generation: generation, page: page, err: err}
	}
}

func resolveConversationCmd(ctx context.Context, app *application.Application, reference string) tea.Cmd {
	return func() tea.Msg {
		id, err := app.Store.ResolveSessionID(ctx, reference)
		if err != nil {
			return errorMsg{err}
		}
		return openSessionCmd(ctx, app, id)()
	}
}

func renameSessionCmd(ctx context.Context, ledger *store.Store, sessionID, title string) tea.Cmd {
	return func() tea.Msg {
		if err := ledger.RenameSession(ctx, sessionID, title); err != nil {
			return errorMsg{err}
		}
		return infoMsg("session renamed")
	}
}

func isTerminal(kind event.Kind) bool {
	return kind == event.FinalCompleted ||
		kind == event.SessionFailed ||
		kind == event.SessionCancelled
}

func sectionStyle(_ bool) lipgloss.Style {
	return lipgloss.NewStyle().Bold(true)
}

func mutedStyle(_ bool) lipgloss.Style {
	return lipgloss.NewStyle().Faint(true)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func last(values []string, count int) []string {
	if len(values) <= count {
		return values
	}
	return values[len(values)-count:]
}
