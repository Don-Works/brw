package browser

import (
	"testing"
	"time"
)

func TestSubscribeTraceDeliversAndCancels(t *testing.T) {
	m := &Manager{}
	entries, cancel := m.SubscribeTrace()

	m.publishTrace(TraceEntry{Action: "click", Ref: "e1", OK: true})
	select {
	case got := <-entries:
		if got.Action != "click" || got.Ref != "e1" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no entry delivered")
	}

	cancel()
	if _, open := <-entries; open {
		t.Fatal("cancel must close the channel")
	}
	// Cancel is idempotent: a second call must not panic on a closed channel.
	cancel()
	// Publishing after the last subscriber left must not panic either.
	m.publishTrace(TraceEntry{Action: "click"})
}

// An observer must never be able to stall the browser. A subscriber that
// stops reading loses its oldest entries; the publisher does not wait.
func TestPublishTraceDropsOldestRatherThanBlocking(t *testing.T) {
	m := &Manager{}
	entries, cancel := m.SubscribeTrace()
	defer cancel()

	total := traceStreamBuffer * 3
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			m.publishTrace(TraceEntry{Action: "click", Ref: "e1", DurationMS: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishTrace blocked on a subscriber that stopped reading")
	}

	if got := len(entries); got > traceStreamBuffer {
		t.Fatalf("buffered %d entries, want at most %d", got, traceStreamBuffer)
	}
	// The survivors must be the NEWEST entries: a live watcher wants what is
	// happening now, not the start of a flow it already missed.
	first := <-entries
	if first.DurationMS < int64(total-traceStreamBuffer-1) {
		t.Fatalf("oldest surviving entry is %d; the buffer kept stale entries instead of recent ones", first.DurationMS)
	}
}

func TestMultipleSubscribersEachSeeEveryEntry(t *testing.T) {
	m := &Manager{}
	a, cancelA := m.SubscribeTrace()
	defer cancelA()
	b, cancelB := m.SubscribeTrace()
	defer cancelB()

	m.publishTrace(TraceEntry{Action: "navigate"})
	for name, ch := range map[string]<-chan TraceEntry{"a": a, "b": b} {
		select {
		case got := <-ch:
			if got.Action != "navigate" {
				t.Fatalf("subscriber %s got %+v", name, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %s received nothing", name)
		}
	}
}
