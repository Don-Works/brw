package bidi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A long session must not grow the event buffer without limit, and the ring
// that bounds it must not cost Await an event it has not seen yet. Sequence
// numbers are what separate the two: a slice index would shift under the drop
// and Await would step over the events the ring pushed forward.
func TestEventRingIsBoundedAndAwaitStillSeesWhatIsLeft(t *testing.T) {
	const flood = maxRecordedEvents * 2
	fake := &fakeBiDi{handle: func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
		send(t, conn, map[string]any{"type": "success", "id": id, "result": map[string]any{}})
		for i := range flood {
			send(t, conn, map[string]any{
				"type":   "event",
				"method": "log.entryAdded",
				"params": map[string]any{"text": fmt.Sprintf("entry-%d", i)},
			})
		}
		// The one the wait is for, last, so it is in the ring while every
		// earlier event has been pushed out of it.
		send(t, conn, map[string]any{
			"type":   "event",
			"method": "browsingContext.load",
			"params": map[string]any{"url": "http://example.invalid/last"},
		})
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, fake.start(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.Command(ctx, "session.subscribe", map[string]any{}, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 20*time.Second)
	defer waitCancel()
	got, err := c.Await(waitCtx, "browsingContext.load", nil)
	if err != nil {
		t.Fatalf("Await lost the event that is still in the ring: %v", err)
	}
	if !strings.Contains(string(got.Params), "/last") {
		t.Fatalf("awaited event params = %s", got.Params)
	}

	if held := len(c.Events()); held > maxRecordedEvents {
		t.Fatalf("the connection holds %d events after %d arrived; the ring caps at %d", held, flood+1, maxRecordedEvents)
	}

	// The oldest entries are the ones that go. Asking for one by its text is
	// how the drop is observed from outside.
	oldest := func() bool {
		for _, ev := range c.Events() {
			if strings.Contains(string(ev.Params), `"entry-0"`) {
				return true
			}
		}
		return false
	}
	if oldest() {
		t.Fatalf("the first of %d events is still held; the ring drops from the front", flood)
	}
}

// Await must not spin when nothing matches: a wait for a method that never
// arrives ends at its deadline, not on the first event in the ring.
func TestAwaitStillTimesOutUnderAFullRing(t *testing.T) {
	fake := &fakeBiDi{handle: func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
		send(t, conn, map[string]any{"type": "success", "id": id, "result": map[string]any{}})
		for i := range maxRecordedEvents + 16 {
			send(t, conn, map[string]any{
				"type":   "event",
				"method": "log.entryAdded",
				"params": map[string]any{"text": fmt.Sprintf("entry-%d", i)},
			})
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, fake.start(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Command(ctx, "session.subscribe", map[string]any{}, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	missCtx, missCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer missCancel()
	if _, err := c.Await(missCtx, "browsingContext.userPromptOpened", nil); err == nil {
		t.Fatal("Await returned an event for a method that never arrived")
	}
}
