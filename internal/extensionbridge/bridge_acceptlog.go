package extensionbridge

import (
	"sync"
	"time"
)

// acceptLogInterval bounds how often one origin's refused connections are
// logged. A foreign extension that retries every few seconds is otherwise one
// log line per attempt for as long as both run: one machine collected 34,701
// lines in ten weeks from a leftover extension nobody saw.
const acceptLogInterval = time.Hour

type acceptLogLimiter struct {
	mu         sync.Mutex
	lastLogged map[string]time.Time
	suppressed map[string]int
}

// admit reports whether a refusal from origin should be logged now, and how
// many refusals from it went unlogged since the last line.
func (l *acceptLogLimiter) admit(origin string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastLogged == nil {
		l.lastLogged = map[string]time.Time{}
		l.suppressed = map[string]int{}
	}
	if last, ok := l.lastLogged[origin]; ok && now.Sub(last) < acceptLogInterval {
		l.suppressed[origin]++
		return false, 0
	}
	skipped := l.suppressed[origin]
	l.lastLogged[origin] = now
	l.suppressed[origin] = 0
	return true, skipped
}
