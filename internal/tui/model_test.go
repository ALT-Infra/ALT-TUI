package tui

import (
	"context"
	"fmt"
	"image/color"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"altv1/internal/application"
	"altv1/internal/event"
	"altv1/internal/nativegui"
	"altv1/internal/profile"
	"altv1/internal/store"
	builtinprofiles "altv1/profiles"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestEnterSubmitsAndKeepsUserPromptVisible(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.input.SetValue("Explain the migration plan.")
	model.input.CursorEnd()

	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil {
		t.Fatal("Enter did not produce a submission command")
	}
	if model.input.Value() != "" {
		t.Fatalf("composer still contains %q after submit", model.input.Value())
	}
	if len(model.turns) != 1 || len(model.turns[0].prompts) != 1 {
		t.Fatalf("submitted prompt was not promoted to a transcript turn: %#v", model.turns)
	}
	rendered := ansi.Strip(model.View().Content)
	if !strings.Contains(rendered, "› Explain the migration plan.") {
		t.Fatalf("submitted prompt is missing from transcript:\n%s", rendered)
	}
	if strings.Contains(rendered, "Ctrl+S") {
		t.Fatalf("obsolete Ctrl+S affordance remains:\n%s", rendered)
	}
}

func TestLaunchOptionsPreserveCodexStyleInitialPromptWithoutInventingATeam(t *testing.T) {
	base, closeApp := testModel(t)
	defer closeApp()
	model := NewWithOptions(context.Background(), base.app, LaunchOptions{
		Workspace: "/tmp/alt-workspace", InitialPrompt: "Inspect this repository.",
	})
	if got := model.workspace; got != "/tmp/alt-workspace" {
		t.Fatalf("workspace = %q", got)
	}
	if got := model.input.Value(); got != "Inspect this repository." {
		t.Fatalf("initial prompt = %q", got)
	}
	if model.starting || model.sessionID != "" || len(model.turns) != 0 {
		t.Fatalf("initial prompt fabricated execution without a Team: starting=%v session=%q turns=%d", model.starting, model.sessionID, len(model.turns))
	}
	if !strings.Contains(model.composerNotice, "Choose a Team") {
		t.Fatalf("initial prompt omitted Team requirement: %q", model.composerNotice)
	}
}

func TestLaunchOptionsOpenResumePickerOrSpecificConversation(t *testing.T) {
	base, closeApp := testModel(t)
	defer closeApp()
	picker := NewWithOptions(context.Background(), base.app, LaunchOptions{ResumePicker: true})
	if !picker.isSessionPicker() || picker.startupCmd == nil {
		t.Fatalf("resume picker was not initialized: picker=%v cmd=%v", picker.isSessionPicker(), picker.startupCmd != nil)
	}

	specific := NewWithOptions(context.Background(), base.app, LaunchOptions{
		ResumeSession: "019abc", InitialPrompt: "Continue with verification.",
	})
	if !specific.starting || specific.startupCmd == nil {
		t.Fatalf("specific resume was not initialized: starting=%v cmd=%v", specific.starting, specific.startupCmd != nil)
	}
	if !reflect.DeepEqual(specific.queued, []string{"Continue with verification."}) {
		t.Fatalf("resume follow-up = %#v", specific.queued)
	}
}

func TestCodexVisualContractAtRest(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	rendered := ansi.Strip(model.View().Content)
	for _, expected := range []string{
		"╭", ">_ ALT", "team:", "directory:", "permissions:",
		"› Ask ALT to do anything", "? for shortcuts",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("Codex-derived visual contract is missing %q:\n%s", expected, rendered)
		}
	}
	for _, obsolete := range []string{"ALT‑V1  profile", "The right Lead", "Orchestration"} {
		if strings.Contains(rendered, obsolete) {
			t.Fatalf("obsolete ALT chrome %q remains:\n%s", obsolete, rendered)
		}
	}
}

func TestCodexLayoutClampsToNarrowTerminalWidth(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 18})
	model = updated.(Model)
	rendered := ansi.Strip(model.View().Content)
	for index, line := range strings.Split(rendered, "\n") {
		if width := ansi.StringWidth(line); width > 40 {
			t.Fatalf("line %d is %d cells wide in a 40-cell terminal: %q", index+1, width, line)
		}
	}
	for _, expected := range []string{"╭", ">_ ALT (vdev)", "› Ask ALT"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("narrow layout lost %q:\n%s", expected, rendered)
		}
	}
}

func TestCodexVisualContractCommandPalette(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.openCommandPalette()
	rendered := ansi.Strip(model.View().Content)
	if !strings.Contains(rendered, "Command palette") || !strings.Contains(rendered, "› New session") {
		t.Fatalf("borderless command palette is missing its title or selection:\n%s", rendered)
	}
	if strings.Contains(rendered, "╭") || strings.Contains(rendered, "╰") {
		t.Fatalf("command palette retained modal border chrome:\n%s", rendered)
	}
	if strings.Contains(rendered, "q quit") || !strings.Contains(rendered, "enter to confirm") {
		t.Fatalf("command palette retained generic widget help:\n%s", rendered)
	}
}

func TestCommandSurfaceHasNoPerConversationModelSwitch(t *testing.T) {
	profiles := 0
	seen := make(map[string]commandDefinition)
	for _, definition := range commandDefinitions {
		seen[definition.command] = definition
		if definition.command == "/model" || strings.HasPrefix(definition.command, "/model ") {
			t.Fatalf("ALT exposed Codex's per-conversation model command: %q", definition.command)
		}
		if definition.command == "/profiles" {
			t.Fatal("duplicate /profiles command remains")
		}
		if definition.command == "/profile" {
			profiles++
		}
	}
	if profiles != 1 {
		t.Fatalf("/profile command count = %d, want exactly one", profiles)
	}
	for _, obsolete := range []string{"/sessions", "/name", "/stop", "/replay"} {
		if _, exists := seen[obsolete]; exists {
			t.Fatalf("obsolete or misleading command remains registered: %s", obsolete)
		}
	}
	if definition, exists := seen["/exit"]; !exists || definition.alias {
		t.Fatalf("/exit must be the canonical exit command: %#v", definition)
	}
	if definition, exists := seen["/quit"]; !exists || !definition.alias {
		t.Fatalf("/quit must remain only as a hidden compatibility alias: %#v", definition)
	}
}

func TestSlashPopupMatchesCodexOrderingAndAliasRules(t *testing.T) {
	commands := func(input string) []string {
		items := commandMatches(input)
		values := make([]string, 0, len(items))
		for _, raw := range items {
			values = append(values, raw.(pickerItem).reference)
		}
		return values
	}

	all := commands("/")
	if slices.Contains(all, "/quit") {
		t.Fatalf("default popup exposed alias /quit: %v", all)
	}
	if !slices.Contains(all, "/exit") {
		t.Fatalf("default popup omitted canonical /exit: %v", all)
	}
	if got := commands("/quit"); !reflect.DeepEqual(got, []string{"/quit"}) {
		t.Fatalf("explicit alias lookup = %v, want [/quit]", got)
	}
	if got := commands("/sum"); len(got) != 0 {
		t.Fatalf("substring-only query produced fuzzy guesses: %v", got)
	}
	if got := commands("/re"); !reflect.DeepEqual(got, []string{"/resume", "/rename"}) {
		t.Fatalf("prefix ordering = %v, want registry order [/resume /rename]", got)
	}
}

func TestStatusRendersCodexStyleTranscriptCardWithALTFields(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.sessionID = "conversation-status"
	model.conversationID = "conversation-status"
	model.turns = []turnView{{
		sessionID: "turn-status", status: store.SessionCompleted,
		tokens: 321, activeDelegations: 1, queuedDelegations: 2,
	}}
	model.currentTurn = 0
	updated, command := model.handleCommand("/status")
	model = updated.(Model)
	if command != nil {
		t.Fatal("/status unexpectedly started backend work")
	}
	rendered := ansi.Strip(model.renderedTranscript)
	for _, expected := range []string{
		"/status", "Team:", "Session:", "Directory:", "Members:",
		"Token usage:", "Permissions:", "conversa", "321",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("status card is missing %q:\n%s", expected, rendered)
		}
	}
	if !strings.Contains(rendered, "╭") || !strings.Contains(rendered, "╰") {
		t.Fatalf("status card is missing Codex history-cell geometry:\n%s", rendered)
	}
	if len(model.statusCards) != 1 {
		t.Fatalf("status snapshots = %d, want 1", len(model.statusCards))
	}
	for index, line := range strings.Split(ansi.Strip(renderStatusSnapshot(model.statusCards[0].snapshot, 40, true)), "\n") {
		if got := ansi.StringWidth(line); got > 40 {
			t.Fatalf("narrow status line %d is %d cells wide: %q", index+1, got, line)
		}
	}
}

func TestStatusCardKeepsItsChronologicalPositionAcrossLaterTurns(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.turns = []turnView{{prompts: []string{"First task"}, status: store.SessionCompleted}}
	model.currentTurn = 0
	updated, _ := model.handleCommand("/status")
	model = updated.(Model)
	model.beginTurn("Second task")
	model.current().status = store.SessionCompleted
	model.touchTranscript(true)
	plain := ansi.Strip(model.renderedTranscript)
	first := strings.Index(plain, "First task")
	status := strings.Index(plain, "/status")
	second := strings.Index(plain, "Second task")
	if !(first >= 0 && first < status && status < second) {
		t.Fatalf("status card lost chronology: first=%d status=%d second=%d\n%s", first, status, second, plain)
	}
}

func TestSlashCommandsDoNotSilentlyIgnoreArguments(t *testing.T) {
	for _, command := range []string{"/exit later", "/cancel later", "/auth later", "/copy later", "/status later"} {
		model, closeApp := testModel(t)
		updated, result := model.handleCommand(command)
		model = updated.(Model)
		closeApp()
		if result != nil {
			t.Fatalf("%q produced a command despite invalid arguments", command)
		}
		if !strings.HasPrefix(model.status, "usage:") {
			t.Fatalf("%q status = %q, want usage error", command, model.status)
		}
	}
}

func TestTeamSurfaceIsOneCommandWithoutForcedArgument(t *testing.T) {
	definition, ok := commandDefinitionFor("/team")
	if !ok || definition.needsInput {
		t.Fatalf("/team definition = %#v, %v", definition, ok)
	}
	if _, obsolete := commandDefinitionFor("/team edit"); obsolete {
		t.Fatal("obsolete split Team command remains registered")
	}
}

func TestSubmissionWithoutTeamPreservesDraftAndNeverStarts(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.profile = nil
	model.input.SetValue("Keep this exact unsent draft.")
	model.input.CursorEnd()

	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command != nil {
		t.Fatal("teamless submission produced a backend command")
	}
	if model.starting || model.sessionID != "" || len(model.turns) != 0 {
		t.Fatalf("teamless submission mutated session state: starting=%v session=%q turns=%d", model.starting, model.sessionID, len(model.turns))
	}
	if got := model.input.Value(); got != "Keep this exact unsent draft." {
		t.Fatalf("teamless submission consumed draft: %q", got)
	}
	rendered := ansi.Strip(model.View().Content)
	warning := "Choose a Team with /profile"
	if !strings.Contains(rendered, warning) {
		t.Fatalf("teamless warning is absent:\n%s", rendered)
	}
	if strings.Index(rendered, warning) > strings.Index(rendered, "› Keep this exact unsent draft.") {
		t.Fatalf("warning is not immediately above the preserved composer:\n%s", rendered)
	}
}

func TestProfilePickerIsBottomPaneRatherThanCenteredModal(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.openProfiles([]store.ProfileSummary{
		{ID: "engineering", Revision: 1, Name: "Engineering Team"},
		{ID: "free", Revision: 1, Name: "Free"},
	})
	if !model.profilePicker {
		t.Fatal("/profile did not select the dedicated bottom-pane surface")
	}
	if height := model.profilePickerHeight(); height >= model.height {
		t.Fatalf("profile picker height %d consumed terminal height %d", height, model.height)
	}
	rendered := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Select Team Profile", "Choose the Team revision", "Engineering Team", "Free", "enter to confirm"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("profile bottom pane is missing %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "Ask ALT to do anything") {
		t.Fatalf("composer remained underneath the profile picker:\n%s", rendered)
	}
}

func TestQuitRemainsAvailableDuringStartupAndAtEmptyComposer(t *testing.T) {
	for _, test := range []struct {
		name     string
		starting bool
		key      tea.Key
	}{
		{name: "startup ctrl-c", starting: true, key: tea.Key{Code: 'c', Mod: tea.ModCtrl}},
		{name: "idle ctrl-d", key: tea.Key{Code: 'd', Mod: tea.ModCtrl}},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, closeApp := testModel(t)
			defer closeApp()
			model.starting = test.starting
			updated, command := model.Update(tea.KeyPressMsg(test.key))
			_ = updated.(Model)
			if command == nil {
				t.Fatal("quit key produced no command")
			}
			if _, ok := command().(tea.QuitMsg); !ok {
				t.Fatalf("quit key produced %T, want tea.QuitMsg", command())
			}
		})
	}
}

func TestSemanticMouseSelectionCopiesUnwrappedPrompt(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 28, Height: 18})
	model = updated.(Model)
	prompt := "Select this sentence even though its rendered form wraps across several terminal rows."
	model.turns = []turnView{{prompts: []string{prompt}, status: store.SessionCompleted}}
	model.currentTurn = 0
	model.touchTranscript(true)
	var block selectionBlock
	for _, candidate := range model.selectionBlocks {
		if candidate.text == prompt {
			block = candidate
			break
		}
	}
	if len(block.lines) < 2 {
		t.Fatalf("wrapped prompt did not produce a multi-line semantic block: %#v", model.selectionBlocks)
	}
	first := block.lines[0]
	last := block.lines[len(block.lines)-1]
	start := first.cells[0]
	finish := last.cells[len(last.cells)-1]
	offset := model.viewport.YOffset()

	updated, _ = model.Update(tea.MouseClickMsg{X: start.x, Y: first.line - offset, Button: tea.MouseLeft})
	model = updated.(Model)
	updated, _ = model.Update(tea.MouseMotionMsg{X: finish.x, Y: last.line - offset, Button: tea.MouseLeft})
	model = updated.(Model)
	if got := model.selectedText(); got != prompt {
		t.Fatalf("semantic selection = %q, want exact unwrapped source %q", got, prompt)
	}
	if !strings.Contains(model.View().Content, "\x1b[7m") {
		t.Fatal("active semantic selection has no visible highlight")
	}
	updated, command := model.Update(tea.MouseReleaseMsg{X: finish.x, Y: last.line - offset, Button: tea.MouseLeft})
	model = updated.(Model)
	if command == nil {
		t.Fatal("mouse release did not issue clipboard commands")
	}
	if model.selection.active {
		t.Fatal("selection remained active after automatic copy")
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("mouse release produced %T, want clipboard and primary-clipboard batch", command())
	}
}

func TestSemanticSelectionMapsLiveComposerToUnderlyingDraft(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 30, Height: 18})
	model = updated.(Model)
	draft := "  A live draft whose logical text must survive visual wrapping exactly."
	model.input.SetValue(draft)
	model.input.CursorEnd()
	model.updateLayout()

	var block selectionBlock
	for _, candidate := range model.selectionBlocksForView() {
		if candidate.text == draft {
			block = candidate
			break
		}
	}
	if len(block.lines) < 2 {
		t.Fatalf("wrapped composer did not expose its semantic source: %#v", block)
	}
	first := block.lines[0]
	last := block.lines[len(block.lines)-1]
	start := first.cells[0]
	finish := last.cells[len(last.cells)-1]
	model.selection.begin(screenPoint{X: start.x, Y: first.line})
	model.selection.extend(screenPoint{X: finish.x, Y: last.line})
	if got := model.selectedText(); got != draft {
		t.Fatalf("composer selection = %q, want exact underlying draft %q", got, draft)
	}
}

func TestCodexActivityCellsPreserveALTOrchestration(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	now := time.Unix(1_700_000_000, 0)
	sessionID := "visual-activity"
	drafts := []event.Draft{
		{Kind: event.SessionCreated, Actor: "user", Data: event.SessionCreatedData{Task: "Repair the parser."}},
		{Kind: event.LeadSelected, Actor: "router", Data: event.LeadSelectedData{LeadID: "engineering", Basis: "The requested result is a code repair."}},
		{Kind: event.DelegationCreated, Actor: "engineering", Data: event.DelegationSpec{MemberID: "research", Objective: "Verify the grammar contract."}},
		{Kind: event.ToolCalled, Actor: "engineering", Data: event.ToolCallData{ToolCallID: "tool-1", Tool: "exec_command", Arguments: `{"cmd":"go test ./..."}`}},
		{Kind: event.ToolCompleted, Actor: "engineering", Data: event.ToolCompletedData{ToolCallID: "tool-1", Tool: "exec_command", Result: `{"output":"ok","running":false,"exit_code":0}`}},
		{Kind: event.FinalStarted, Actor: "engineering"},
		{Kind: event.FinalCompleted, Actor: "engineering", Data: event.FinalCompletedData{Answer: "Implemented and verified."}},
	}
	for index, draft := range drafts {
		item, err := draft.Materialize(sessionID, int64(index+1), now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		model.applyEvent(item)
	}
	model.touchTranscript(true)
	rendered := ansi.Strip(model.View().Content)
	for _, expected := range []string{
		"› Repair the parser.",
		"• Routed to engineering",
		"└ The requested result is a code repair.",
		"• Delegated to research",
		"└ Verify the grammar contract.",
		"• Ran go test ./...",
		"└ ok",
		"• Lead synthesis started",
		"Implemented and verified.",
		"─ Worked for 6s",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("activity cell contract is missing %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "Orchestration") {
		t.Fatalf("activity retained the old diagnostic-dump heading:\n%s", rendered)
	}
}

func TestUserSurfaceDerivesCodexContrastBandFromTerminalBackground(t *testing.T) {
	for _, test := range []struct {
		name       string
		background color.Color
		want       color.RGBA
	}{
		{name: "dark", background: color.RGBA{A: 255}, want: color.RGBA{R: 31, G: 31, B: 31, A: 255}},
		{name: "light", background: color.RGBA{R: 255, G: 255, B: 255, A: 255}, want: color.RGBA{R: 245, G: 245, B: 245, A: 255}},
	} {
		t.Run(test.name, func(t *testing.T) {
			background := userSurfaceStyle(test.background).GetBackground()
			red, green, blue, alpha := background.RGBA()
			got := color.RGBA{R: uint8(red >> 8), G: uint8(green >> 8), B: uint8(blue >> 8), A: uint8(alpha >> 8)}
			if got != test.want {
				t.Fatalf("contrast band = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestTerminalBackgroundEventReachesComposerSurface(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	updated, _ := model.Update(tea.BackgroundColorMsg{Color: color.RGBA{A: 255}})
	model = updated.(Model)
	if model.background == nil {
		t.Fatal("terminal background reply was not retained")
	}
	want := color.RGBA{R: 31, G: 31, B: 31, A: 255}
	styles := model.input.Styles()
	backgrounds := map[string]color.Color{
		"focused base":        styles.Focused.Base.GetBackground(),
		"focused text":        styles.Focused.Text.GetBackground(),
		"focused cursor line": styles.Focused.CursorLine.GetBackground(),
		"focused placeholder": styles.Focused.Placeholder.GetBackground(),
		"focused prompt":      styles.Focused.Prompt.GetBackground(),
		"blurred base":        styles.Blurred.Base.GetBackground(),
		"blurred text":        styles.Blurred.Text.GetBackground(),
		"blurred cursor line": styles.Blurred.CursorLine.GetBackground(),
		"blurred placeholder": styles.Blurred.Placeholder.GetBackground(),
		"blurred prompt":      styles.Blurred.Prompt.GetBackground(),
	}
	for name, background := range backgrounds {
		if got := rgba8(background); got != want {
			t.Errorf("%s background = %#v, want %#v", name, got, want)
		}
	}
	inner := model.input.View()
	if !strings.Contains(inner, "\x1b[48;2;31;31;31m") {
		t.Fatalf("textarea render does not emit the derived surface color: %q", inner)
	}
	if strings.Contains(inner, "\x1b[48;5;0m") || strings.Contains(inner, "\x1b[48;2;0;0;0m") {
		t.Fatalf("textarea cursor row repainted the surface black: %q", inner)
	}
}

func rgba8(value color.Color) color.RGBA {
	if value == nil {
		return color.RGBA{}
	}
	red, green, blue, alpha := value.RGBA()
	return color.RGBA{R: uint8(red >> 8), G: uint8(green >> 8), B: uint8(blue >> 8), A: uint8(alpha >> 8)}
}

func TestToolCellsShowSemanticSummaryAndCompleteTranscript(t *testing.T) {
	command := &toolActivity{
		actor: "engineering",
		call: event.ToolCallData{
			ToolCallID: "call-command", Tool: "exec_command",
			Arguments: `{"cmd":"go test ./..."}`,
		},
		completion: &event.ToolCompletedData{
			ToolCallID: "call-command", Tool: "exec_command",
			Result: `{"output":"one\ntwo\nthree\nfour\nfive\nsix\nseven","running":false,"exit_code":0}`,
		},
	}
	compact := ansi.Strip(renderToolActivity(command, 80, true, false))
	for _, expected := range []string{"• Ran go test ./...", "… +3 lines", "└ seven"} {
		if !strings.Contains(compact, expected) {
			t.Fatalf("compact command cell is missing %q:\n%s", expected, compact)
		}
	}
	if strings.Contains(compact, "four") {
		t.Fatalf("compact command cell did not collapse middle output:\n%s", compact)
	}
	expanded := ansi.Strip(renderToolActivity(command, 80, true, true))
	for _, expected := range []string{"four", "five", "six"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("complete transcript is missing %q:\n%s", expected, expanded)
		}
	}

	patch := &toolActivity{
		call: event.ToolCallData{
			ToolCallID: "call-patch", Tool: "apply_patch",
			Arguments: `{"patch":"--- a/internal/tui/model.go\n+++ b/internal/tui/model.go\n@@ -1,2 +1,3 @@\n-old\n+new\n+more\n keep"}`,
		},
		completion: &event.ToolCompletedData{
			ToolCallID: "call-patch", Tool: "apply_patch",
			Result: `{"changed":["internal/tui/model.go"]}`,
		},
	}
	cell := ansi.Strip(renderToolActivity(patch, 80, true, false))
	if !strings.Contains(cell, "• Edited internal/tui/model.go (+2 -1)") {
		t.Fatalf("patch cell does not expose deterministic file deltas:\n%s", cell)
	}
}

func TestWriteStdinCoalescesIntoOriginatingCommandCell(t *testing.T) {
	var turn turnView
	turn.recordToolCall("engineering", event.ToolCallData{
		ToolCallID: "exec-1", Tool: "exec_command", Arguments: `{"cmd":"make test"}`,
	})
	turn.recordToolCompletion("engineering", event.ToolCompletedData{
		ToolCallID: "exec-1", Tool: "exec_command",
		Result: `{"session_id":42317,"output":"first\n","running":true}`,
	})
	turn.recordToolCall("engineering", event.ToolCallData{
		ToolCallID: "poll-1", Tool: "write_stdin", Arguments: `{"session_id":42317}`,
	})
	turn.recordToolCompletion("engineering", event.ToolCompletedData{
		ToolCallID: "poll-1", Tool: "write_stdin",
		Result: `{"output":"second\n","running":false,"exit_code":0}`,
	})
	if len(turn.timeline) != 1 {
		t.Fatalf("one process rendered as %d timeline cells", len(turn.timeline))
	}
	activity := turn.toolActivities["exec-1"]
	rendered := ansi.Strip(renderToolActivity(activity, 80, true, false))
	for _, expected := range []string{"• Ran make test", "first", "second"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("coalesced command cell is missing %q:\n%s", expected, rendered)
		}
	}
}

func TestTeamCommandTargetsPinnedRevisionInReadOnlyNativeMode(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()

	launch := selectedTeamInspectorLaunch(model.profile)
	if launch.Mode != nativegui.ModeTeam {
		t.Fatalf("mode = %q, want %q", launch.Mode, nativegui.ModeTeam)
	}
	if launch.ProfileID != model.profile.Profile.ID ||
		launch.Revision != model.profile.Profile.Revision {
		t.Fatalf("launch does not pin selected Team revision: %#v", launch)
	}

	updated, command := model.handleCommand("/team")
	model = updated.(Model)
	if command == nil {
		t.Fatal("/team did not launch the native inspector")
	}
	if model.status != "opening Team graph" {
		t.Fatalf("status = %q", model.status)
	}
}

func TestMouseWheelIsAppliedOnceWithoutAViewFeedbackCallback(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()

	model.viewport.SetContent(strings.Repeat("transcript line\n", 100))
	model.viewport.SetYOffset(30)
	rendered := model.View()
	if rendered.OnMouse != nil {
		t.Fatal("TUI view can re-enqueue a mouse event through OnMouse")
	}

	updated, command := model.Update(tea.MouseWheelMsg(tea.Mouse{
		Button: tea.MouseWheelUp,
	}))
	model = updated.(Model)
	if command != nil {
		t.Fatal("one wheel event emitted a follow-up command")
	}
	if got, want := model.viewport.YOffset(), 27; got != want {
		t.Fatalf("one wheel event moved to offset %d, want exactly %d", got, want)
	}
}

func TestCtrlJInsertsNewlineWithoutSubmitting(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.input.SetValue("first line")
	model.input.CursorEnd()

	updated, _ := model.Update(tea.KeyPressMsg(tea.Key{
		Code: 'j',
		Mod:  tea.ModCtrl,
	}))
	model = updated.(Model)
	if model.input.Value() != "first line\n" {
		t.Fatalf("Ctrl+J produced %q, want a newline", model.input.Value())
	}
	if len(model.turns) != 0 {
		t.Fatal("Ctrl+J unexpectedly submitted the draft")
	}
}

func TestPromptSubmittedDuringSessionCreationQueuesInsteadOfStartingAnotherRun(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.input.SetValue("First task.")
	model.input.CursorEnd()
	updated, firstCommand := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if firstCommand == nil || !model.starting {
		t.Fatal("first submission did not enter the durable-start boundary")
	}
	model.input.SetValue("Follow after that.")
	model.input.CursorEnd()
	updated, secondCommand := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if secondCommand != nil {
		t.Fatal("second submission started a concurrent session while the first was being created")
	}
	if len(model.queued) != 1 || model.queued[0] != "Follow after that." {
		t.Fatalf("second submission was not queued: %#v", model.queued)
	}
}

func TestAltUpRestoresLastQueuedPromptForEditing(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.queued = []string{"first queued", "second queued"}
	updated, _ := model.Update(tea.KeyPressMsg(tea.Key{
		Code: tea.KeyUp,
		Mod:  tea.ModAlt,
	}))
	model = updated.(Model)
	if model.input.Value() != "second queued" {
		t.Fatalf("composer = %q, want most recent queued prompt", model.input.Value())
	}
	if len(model.queued) != 1 || model.queued[0] != "first queued" {
		t.Fatalf("remaining queue = %#v", model.queued)
	}
}

func TestInterruptedRunRestoresQueuedPromptsInsteadOfAutoSubmitting(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	sessionID := "interrupted-turn"
	model.sessionID = sessionID
	model.turns = []turnView{{sessionID: sessionID, status: "running"}}
	model.currentTurn = 0
	model.queued = []string{"first follow-up", "second follow-up"}
	cancelled, err := (event.Draft{
		Kind: event.SessionCancelled, Actor: "user",
		Data: event.FailureData{Error: "interrupted"},
	}).Materialize(sessionID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	updated, command := model.Update(eventMsg{event: cancelled, ok: true})
	model = updated.(Model)
	if command != nil {
		t.Fatal("cancelled run automatically submitted queued work")
	}
	if len(model.queued) != 0 {
		t.Fatalf("queue was not restored: %#v", model.queued)
	}
	for _, expected := range []string{"first follow-up", "second follow-up"} {
		if !strings.Contains(model.input.Value(), expected) {
			t.Fatalf("restored draft is missing %q: %q", expected, model.input.Value())
		}
	}
}

func TestCompletionNotifiesOnlyWhenTerminalIsUnfocusedAndNoQueueRemains(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	sessionID := "background-turn"
	model.focused = false
	model.turns = []turnView{{sessionID: sessionID, status: "running"}}
	model.currentTurn = 0
	completed, err := (event.Draft{
		Kind: event.FinalCompleted, Actor: "lead",
		Data: event.FinalCompletedData{Answer: "Finished in the background."},
	}).Materialize(sessionID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, command := model.Update(eventMsg{event: completed, ok: true})
	if command == nil {
		t.Fatal("unfocused completion did not request a terminal notification")
	}
	raw, ok := command().(tea.RawMsg)
	if !ok || !strings.Contains(raw.Msg.(string), "ALT completed") {
		t.Fatalf("notification = %#v", raw)
	}

	model.queued = []string{"continue automatically"}
	if command := model.notificationCmd(completed); command != nil {
		t.Fatal("completion notified even though queued work would continue")
	}
}

func TestReplayReconstructsUserPromptAndMarkdownAnswer(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	sessionID := "01900000-0000-7000-8000-000000000001"
	created, err := (event.Draft{
		Kind:  event.SessionCreated,
		Actor: "user",
		Data:  event.SessionCreatedData{Task: "Review this code."},
	}).Materialize(sessionID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	completed, err := (event.Draft{
		Kind:  event.FinalCompleted,
		Actor: "lead",
		Data:  event.FinalCompletedData{Answer: "**Done.**"},
	}).Materialize(sessionID, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	model.applyEvent(created)
	model.applyEvent(completed)
	model.touchTranscript(true)
	rendered := ansi.Strip(model.View().Content)
	if !strings.Contains(rendered, "› Review this code.") {
		t.Fatalf("replayed user prompt missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Done.") || strings.Contains(rendered, "**Done.**") {
		t.Fatalf("answer was not rendered as Markdown:\n%s", rendered)
	}
}

func TestReplaySeparatesDurableConversationTurns(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	now := time.Now()
	for index, task := range []string{"Initial request.", "A richer follow-up."} {
		sessionID := "turn-" + string(rune('a'+index))
		created, err := (event.Draft{
			Kind: event.SessionCreated, Actor: "user",
			Data: event.SessionCreatedData{Task: task},
		}).Materialize(sessionID, 1, now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		completed, err := (event.Draft{
			Kind: event.FinalCompleted, Actor: "lead",
			Data: event.FinalCompletedData{Answer: "Answer " + task},
		}).Materialize(sessionID, 2, now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		model.applyEvent(created)
		model.applyEvent(completed)
	}
	if len(model.turns) != 2 {
		t.Fatalf("replay produced %d turns, want 2: %#v", len(model.turns), model.turns)
	}
	model.touchTranscript(true)
	rendered := ansi.Strip(model.View().Content)
	for _, text := range []string{"› Initial request.", "› A richer follow-up."} {
		if !strings.Contains(rendered, text) {
			t.Fatalf("conversation is missing %q:\n%s", text, rendered)
		}
	}
}

func TestSlashOpensContextualCommandPopup(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.input.SetValue("/sta")
	model.input.CursorEnd()
	model.syncSlashPopup()

	if model.slashPopup == nil {
		t.Fatal("slash command popup did not open")
	}
	selected, ok := model.slashPopup.SelectedItem().(pickerItem)
	if !ok || selected.reference != "/status" {
		t.Fatalf("selected slash item = %#v, want /status", model.slashPopup.SelectedItem())
	}
}

func TestAuthOpensRegisteredGatewayPicker(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()

	updated, command := model.handleCommand("/auth")
	model = updated.(Model)
	if command != nil {
		t.Fatal("/auth started a credential process before the user selected a gateway")
	}
	if model.picker == nil {
		t.Fatal("/auth did not open the gateway picker")
	}
	descriptors := model.app.Providers.Descriptors()
	if got := len(model.picker.Items()); got != len(descriptors)+1 {
		t.Fatalf("connection picker contains %d entries, want %d", got, len(descriptors)+1)
	}
	for index, descriptor := range descriptors {
		item, ok := model.picker.Items()[index].(pickerItem)
		if !ok {
			t.Fatalf("gateway picker entry %d has type %T", index, model.picker.Items()[index])
		}
		if item.kind != "gateway" || item.reference != descriptor.ID {
			t.Fatalf("gateway picker entry %d = %#v, want %s", index, item, descriptor.ID)
		}
	}
	exa, ok := model.picker.Items()[len(descriptors)].(pickerItem)
	if !ok || exa.kind != "gateway" || exa.reference != "exa" {
		t.Fatalf("Exa connection entry = %#v", model.picker.Items()[len(descriptors)])
	}
}

func TestPromptHistoryPreservesDraftAtNewerBoundary(t *testing.T) {
	var history promptHistory
	history.record("first")
	history.record("second")

	value, ok, lookup := history.older("unfinished draft")
	if lookup != nil {
		t.Fatalf("local history unexpectedly requested persistent offset: %#v", lookup)
	}
	if !ok || value != "second" {
		t.Fatalf("older = (%q, %t), want second", value, ok)
	}
	value, ok = history.newer()
	if !ok || value != "unfinished draft" {
		t.Fatalf("newer = (%q, %t), want original draft", value, ok)
	}
}

func TestPromptHistoryRejectsStaleOffsetReply(t *testing.T) {
	var history promptHistory
	history.initialize(store.PromptSnapshot{Count: 2, NewestID: "snapshot"})

	_, _, first := history.older("draft")
	if first == nil {
		t.Fatal("persistent history did not request its first offset")
	}
	if _, ok := history.newer(); !ok {
		t.Fatal("newer did not restore the draft while lookup was pending")
	}
	_, _, second := history.older("new draft")
	if second == nil || second.token == first.token {
		t.Fatalf("second lookup = %#v after first %#v", second, first)
	}
	if value, accepted := history.resolve(first.token, "stale", true); accepted {
		t.Fatalf("stale reply changed history to %q", value)
	}
	if value, accepted := history.resolve(second.token, "newest durable", true); !accepted || value != "newest durable" {
		t.Fatalf("current reply = (%q, %t)", value, accepted)
	}
}

func TestComposerRetainsDraftBeyondFormerContentCeiling(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.width = 100
	model.height = 30
	lines := make([]string, 350)
	for index := range lines {
		lines[index] = fmt.Sprintf("line %03d", index)
	}
	draft := strings.Join(lines, "\n")
	updated, _ := model.Update(tea.PasteMsg{Content: draft})
	model = updated.(Model)

	if got := model.input.Value(); got != draft {
		t.Fatalf(
			"textarea retained %d/%d bytes after a 350-line paste",
			len(got),
			len(draft),
		)
	}
	if model.input.Height() >= model.height {
		t.Fatalf("visible editor height %d consumed terminal height %d", model.input.Height(), model.height)
	}
	if model.viewport.Height() < 1 {
		t.Fatal("measured editor geometry left no transcript viewport")
	}
}

func TestTimelineProjectionDoesNotSilentlyDiscardOlderEvents(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	now := time.Now()
	created, err := (event.Draft{
		Kind: event.SessionCreated,
		Data: event.SessionCreatedData{Task: "retain the complete projection"},
	}).Materialize("large-timeline", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	model.applyEvent(created)
	const count = 620
	for index := 0; index < count; index++ {
		item, err := (event.Draft{
			Kind:  event.LeadSelected,
			Actor: "router",
			Data: event.LeadSelectedData{
				LeadID: "engineering",
				Basis:  fmt.Sprintf("selection %03d", index),
			},
		}).Materialize("large-timeline", int64(index+2), now)
		if err != nil {
			t.Fatal(err)
		}
		model.applyEvent(item)
	}
	if got := len(model.current().timeline); got != count {
		t.Fatalf("timeline retained %d/%d event projections", got, count)
	}
	if !strings.Contains(model.current().timeline[0], "selection 000") {
		t.Fatalf("oldest timeline item was replaced: %q", model.current().timeline[0])
	}
}

func TestPagedPickerRejectsResponseFromSupersededRequest(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.width = 100
	model.height = 30
	_ = model.openPagedPicker("session", "Sessions")
	staleGeneration := model.pickerPage.generation
	_ = model.openPagedPicker("history", "Prompt history")

	model.applySessionPage(sessionPageMsg{
		generation: staleGeneration,
		page: store.SessionPage{Items: []store.Session{{
			ID: "stale-session", ConversationID: "stale-conversation", Title: "stale",
		}}},
	})
	if got := len(model.picker.Items()); got != 0 {
		t.Fatalf("superseded session response inserted %d items into prompt picker", got)
	}
	if model.pickerPage.kind != "history" {
		t.Fatalf("superseded response changed picker kind to %q", model.pickerPage.kind)
	}
}

func TestResumeUsesCodexFullScreenSessionBrowser(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	model.width = 100
	model.height = 24
	_ = model.openPagedPicker("session", "Resume a previous session")
	reference := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	model.pickerPage.referenceTime = reference
	model.applySessionPage(sessionPageMsg{
		generation: model.pickerPage.generation,
		page: store.SessionPage{Items: []store.Session{
			{
				ID: "turn-1", ConversationID: "conversation-1",
				Title: "Repair process recovery", Workspace: "/workspace/ALT-TUI",
				ProfileID: "free", ProfileRevision: 1, Status: store.SessionCompleted,
				UpdatedAt: reference.Add(-42 * time.Second),
			},
			{
				ID: "turn-2", ConversationID: "conversation-2",
				Title: "Inspect graph layout", Workspace: "/workspace/ALT-TUI",
				ProfileID: "free", ProfileRevision: 1, Status: store.SessionFailed,
				UpdatedAt: reference.Add(-35 * time.Minute),
			},
		}},
	})

	raw := model.View().Content
	rendered := ansi.Strip(raw)
	for _, expected := range []string{
		"Resume a previous session", "Type to search", "Sort: [Updated]",
		"❯ Repair process recovery", "42s ago", "free@1 · completed",
		"enter resume", "esc start new", "ctrl+c quit", "↑/↓ browse",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("resume browser is missing %q:\n%s", expected, rendered)
		}
	}
	for _, forbidden := range []string{"Sessions · / filters", "enter to confirm", "Ask ALT to do anything"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("resume browser retained generic picker chrome %q:\n%s", forbidden, rendered)
		}
	}
	if strings.Contains(raw, "\x1b[48;") {
		t.Fatalf("resume browser introduced a modal background: %q", raw)
	}
	if got := strings.Count(rendered, "\n") + 1; got != model.height {
		t.Fatalf("resume browser height = %d, want terminal height %d\n%s", got, model.height, rendered)
	}
}

func TestResumeSearchAcceptsPlainTypingAndSessionsAliasIsAbsent(t *testing.T) {
	for _, definition := range commandDefinitions {
		if definition.command == "/sessions" {
			t.Fatal("obsolete /sessions alias remains registered")
		}
	}
	definition, ok := commandDefinitionFor("/resume")
	if !ok || definition.needsInput {
		t.Fatalf("/resume definition = %#v, %v; it must open directly", definition, ok)
	}

	model, closeApp := testModel(t)
	defer closeApp()
	_ = model.openPagedPicker("session", "Resume a previous session")
	updated, _ := model.Update(tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'}))
	model = updated.(Model)
	if got := model.picker.FilterInput.Value(); got != "r" {
		t.Fatalf("plain typing produced search %q, want r", got)
	}
	if !strings.Contains(ansi.Strip(model.View().Content), "Search: r") {
		t.Fatalf("active search is absent:\n%s", ansi.Strip(model.View().Content))
	}
	updated, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	model = updated.(Model)
	if got := model.picker.FilterInput.Value(); got != "" {
		t.Fatalf("escape left search text %q", got)
	}
	if !model.isSessionPicker() {
		t.Fatal("first escape closed the browser instead of clearing search")
	}
}

func TestTerminalTextRejectsEscapeAndBidiInjection(t *testing.T) {
	input := "safe\x1b[31m red\x1b[0m \u202Ehidden\nnext"
	if got := sanitizeTerminalText(input); got != "safe red hidden\nnext" {
		t.Fatalf("sanitized terminal text = %q", got)
	}
	if got := sanitizeTerminalTitle(input); got != "safe red hidden next" {
		t.Fatalf("sanitized terminal title = %q", got)
	}
}

func TestPersistedSteerDoesNotDuplicateOptimisticUserCell(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	sessionID := "active-turn"
	model.turns = []turnView{{
		sessionID: sessionID,
		prompts:   []string{"Initial task.", "Focus on recovery."},
	}}
	model.currentTurn = 0
	model.optimisticSteers = []string{"Focus on recovery."}
	instruction, err := (event.Draft{
		Kind: event.UserInstruction, Actor: "user",
		Data: event.UserInstructionData{Text: "Focus on recovery."},
	}).Materialize(sessionID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	model.applyEvent(instruction)
	if len(model.turns[0].prompts) != 2 {
		t.Fatalf("persisted steer duplicated the prompt: %#v", model.turns[0].prompts)
	}
	if len(model.optimisticSteers) != 0 {
		t.Fatalf("optimistic steer acknowledgement was not consumed: %#v", model.optimisticSteers)
	}
}

func TestLeadTurnBoundaryClearsConsumedSteerPreview(t *testing.T) {
	model, closeApp := testModel(t)
	defer closeApp()
	sessionID := "active-turn"
	model.turns = []turnView{{sessionID: sessionID, status: "running"}}
	model.currentTurn = 0
	model.pendingSteers = []string{"Focus on recovery."}
	started, err := (event.Draft{
		Kind: event.LeadTurnStarted, Actor: "lead",
		Data: event.LeadTurnData{
			Turn: 2, SignalKinds: []string{string(event.UserInstruction)},
		},
	}).Materialize(sessionID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	model.applyEvent(started)
	if len(model.pendingSteers) != 0 {
		t.Fatalf("consumed steer still shown as pending: %#v", model.pendingSteers)
	}
}

func testModel(t *testing.T) (Model, func()) {
	t.Helper()
	app, err := application.OpenAt(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := New(context.Background(), app)
	document, err := profile.Parse(builtinprofiles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Store.ImportProfile(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	model.profile = document
	model.status = "ready"
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	model = updated.(Model)
	return model, func() {
		if err := app.Close(); err != nil {
			t.Errorf("close app: %v", err)
		}
	}
}
