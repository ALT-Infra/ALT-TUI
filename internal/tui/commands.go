package tui

import (
	"strings"

	"charm.land/bubbles/v2/list"
)

type commandDefinition struct {
	command     string
	description string
	needsInput  bool
	alias       bool
}

var commandDefinitions = []commandDefinition{
	{command: "/new", description: "start a fresh transcript"},
	{command: "/profile", description: "choose a Team Profile"},
	{command: "/team", description: "create, edit, or inspect a Team in one graph window"},
	{command: "/auth", description: "configure a gateway credential securely"},
	{command: "/research", description: "choose the web research provider"},
	{command: "/resume", description: "resume a saved conversation"},
	{command: "/rename", description: "rename the active session", needsInput: true},
	{command: "/copy", description: "copy the last answer as Markdown"},
	{command: "/status", description: "show current session and usage"},
	{command: "/cancel", description: "interrupt the active orchestration"},
	{command: "/thinking", description: "toggle the graphical reasoning surface"},
	{command: "/clear", description: "clear the transcript and start fresh"},
	{command: "/exit", description: "exit ALT"},
	{command: "/quit", description: "exit ALT", alias: true},
}

func commandMatches(input string) []list.Item {
	query := strings.TrimPrefix(strings.TrimSpace(strings.Split(input, "\n")[0]), "/")
	query = strings.ToLower(query)
	var exact, prefix []commandDefinition
	for _, definition := range commandDefinitions {
		name := strings.TrimPrefix(definition.command, "/")
		lower := strings.ToLower(name)
		switch {
		case query == "":
			if !definition.alias {
				prefix = append(prefix, definition)
			}
		case lower == query:
			exact = append(exact, definition)
		case strings.HasPrefix(lower, query):
			prefix = append(prefix, definition)
		}
	}
	ordered := append(exact, prefix...)
	items := make([]list.Item, 0, len(ordered))
	for _, definition := range ordered {
		items = append(items, pickerItem{
			kind:        "slash-command",
			reference:   definition.command,
			title:       definition.command,
			description: definition.description,
		})
	}
	return items
}

func commandDefinitionFor(value string) (commandDefinition, bool) {
	value = strings.TrimSpace(value)
	for _, definition := range commandDefinitions {
		if value == definition.command ||
			(definition.needsInput && strings.HasPrefix(value, definition.command+" ")) {
			return definition, true
		}
	}
	return commandDefinition{}, false
}

func newInlineCommandPopup(items []list.Item, width int) *list.Model {
	height := min(8, max(1, len(items)))
	popup := list.New(items, codexListDelegate{inline: true}, max(30, width), height)
	popup.SetShowTitle(false)
	popup.SetShowFilter(false)
	popup.SetShowHelp(false)
	popup.SetShowPagination(false)
	popup.SetShowStatusBar(false)
	popup.SetFilteringEnabled(false)
	popup.DisableQuitKeybindings()
	styleCodexPicker(&popup)
	return &popup
}
