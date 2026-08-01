package tui

import (
	"encoding/json"
	"io"
	"sync"
	"testing"

	"altv1/internal/event"
)

func TestThinkingChildQueueDoesNotDropBurstEvents(t *testing.T) {
	reader, writer := io.Pipe()
	process := &nativeProcess{stdin: writer}
	process.updateReady = sync.NewCond(&process.updateMu)
	done := make(chan struct{})
	go func() {
		process.writeEvents()
		close(done)
	}()

	const total = 5000
	for sequence := 1; sequence <= total; sequence++ {
		process.pushEvent(event.Event{SessionID: "turn", Sequence: int64(sequence)})
	}
	process.closeUpdates()

	decoder := json.NewDecoder(reader)
	for expected := 1; expected <= total; expected++ {
		var item event.Event
		if err := decoder.Decode(&item); err != nil {
			t.Fatalf("decode event %d: %v", expected, err)
		}
		if item.Sequence != int64(expected) {
			t.Fatalf("event %d arrived as sequence %d", expected, item.Sequence)
		}
	}
	var extra event.Event
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("event stream did not terminate exactly after %d events: %v", total, err)
	}
	<-done
}
