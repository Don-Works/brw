package browser

import "sync"

// traceStreamBuffer is how many entries a slow subscriber may fall behind
// before it starts losing the oldest. Small on purpose: a live watcher wants
// what is happening now, and an observer must never be able to stall the
// browser by reading slowly.
const traceStreamBuffer = 64

type traceSubscriber struct {
	ch chan TraceEntry
}

// SubscribeTrace returns a channel of actions as they happen, and a function
// that unsubscribes and closes it.
//
// Entries arrive AFTER recordTrace has resolved the ref's semantic identity
// and applied credential redaction, so a subscriber inherits both. A live
// stream that tapped the raw call sites instead would be the one path where a
// typed password escaped, which is why this hangs off the single choke point
// every action already goes through.
func (m *Manager) SubscribeTrace() (<-chan TraceEntry, func()) {
	sub := &traceSubscriber{ch: make(chan TraceEntry, traceStreamBuffer)}
	m.streamMu.Lock()
	if m.streamSubs == nil {
		m.streamSubs = make(map[*traceSubscriber]struct{})
	}
	m.streamSubs[sub] = struct{}{}
	m.streamMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.streamMu.Lock()
			delete(m.streamSubs, sub)
			m.streamMu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// publishTrace fans one recorded entry out to live subscribers. Never blocks:
// a subscriber that cannot keep up drops its oldest entry rather than holding
// up the action that produced it.
func (m *Manager) publishTrace(entry TraceEntry) {
	m.streamMu.RLock()
	defer m.streamMu.RUnlock()
	for sub := range m.streamSubs {
		select {
		case sub.ch <- entry:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- entry:
			default:
			}
		}
	}
}
