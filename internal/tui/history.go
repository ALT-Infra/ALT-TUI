package tui

import "altv1/internal/store"

type promptLookup struct {
	token    uint64
	snapshot store.PromptSnapshot
	offset   int
}

// promptHistory keeps prompts submitted during this TUI process locally and
// addresses older durable prompts by offset in a pinned database snapshot.
// It therefore has no "oldest visible prompt" ceiling and does not let newer
// database writes shift an in-progress navigation cursor.
type promptHistory struct {
	snapshot   store.PromptSnapshot
	persistent map[int]string
	local      []string
	cursor     int
	draft      string
	active     bool
	nextToken  uint64
	pending    *promptLookup
}

func (h *promptHistory) initialize(snapshot store.PromptSnapshot) {
	h.snapshot = snapshot
	if h.persistent == nil {
		h.persistent = make(map[int]string)
	}
	h.resetNavigation()
}

func (h *promptHistory) record(value string) {
	if value == "" {
		return
	}
	if len(h.local) > 0 && h.local[len(h.local)-1] == value {
		h.resetNavigation()
		return
	}
	h.local = append(h.local, value)
	h.resetNavigation()
}

func (h *promptHistory) older(current string) (string, bool, *promptLookup) {
	if h.pending != nil {
		return "", false, nil
	}
	if !h.active {
		h.draft = current
		h.cursor = -1
		h.active = true
	}
	target := h.cursor + 1
	if target >= len(h.local)+h.snapshot.Count {
		value, ok := h.valueAt(h.cursor)
		return value, ok, nil
	}
	if value, ok := h.valueAt(target); ok {
		h.cursor = target
		return value, true, nil
	}

	offset := target - len(h.local)
	h.nextToken++
	lookup := &promptLookup{
		token:    h.nextToken,
		snapshot: h.snapshot,
		offset:   offset,
	}
	h.pending = lookup
	return "", false, lookup
}

func (h *promptHistory) resolve(
	token uint64,
	prompt string,
	found bool,
) (string, bool) {
	if h.pending == nil || h.pending.token != token {
		return "", false
	}
	target := len(h.local) + h.pending.offset
	offset := h.pending.offset
	h.pending = nil
	if !found {
		return "", false
	}
	if h.persistent == nil {
		h.persistent = make(map[int]string)
	}
	h.persistent[offset] = prompt
	h.cursor = target
	h.active = true
	return prompt, true
}

func (h *promptHistory) newer() (string, bool) {
	if h.pending != nil {
		// A later navigation intent makes the outstanding reply stale.
		h.pending = nil
	}
	if !h.active {
		return "", false
	}
	if h.cursor > 0 {
		h.cursor--
		return h.valueAt(h.cursor)
	}
	draft := h.draft
	h.resetNavigation()
	return draft, true
}

func (h *promptHistory) valueAt(index int) (string, bool) {
	if index < 0 {
		return "", false
	}
	if index < len(h.local) {
		return h.local[len(h.local)-1-index], true
	}
	value, ok := h.persistent[index-len(h.local)]
	return value, ok
}

func (h *promptHistory) resetNavigation() {
	h.cursor = -1
	h.draft = ""
	h.active = false
	h.pending = nil
}
