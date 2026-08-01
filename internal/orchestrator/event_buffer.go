package orchestrator

import "strings"

type eventTextBuffer struct {
	limit int
	value strings.Builder
	emit  func(string) error
}

func newEventTextBuffer(limit int, emit func(string) error) *eventTextBuffer {
	return &eventTextBuffer{limit: limit, emit: emit}
}

func (b *eventTextBuffer) Add(value string) error {
	if value == "" {
		return nil
	}
	b.value.WriteString(value)
	if b.value.Len() < b.limit {
		return nil
	}
	return b.Flush()
}

func (b *eventTextBuffer) Flush() error {
	if b.value.Len() == 0 {
		return nil
	}
	value := b.value.String()
	b.value.Reset()
	return b.emit(value)
}
