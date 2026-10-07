package extensionbridge

import (
	"sync"
	"time"
)

const acceptLogInterval = time.Hour

type acceptLogLimiter struct {
	mu         sync.Mutex
	lastLogged map[string]time.Time
	suppressed map[string]int
}

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
