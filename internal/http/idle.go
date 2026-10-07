package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

var idleExemptPrefixes = map[string]string{
	"/health": "a supervisor's liveness poll is not work; counting it would keep an abandoned daemon alive forever",
}

func countsAsActivity(path string) bool {
	for prefix := range idleExemptPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	return true
}

type idleTracker struct {
	mu     sync.Mutex
	after  time.Duration
	last   time.Time
	inWork int
}

// SetIdleExit arms an idle shutdown after d without a request that counts as work.
func (s *Server) SetIdleExit(d time.Duration) {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	s.idle.after = d
	s.idle.last = time.Now()
}

// IdleExit reports the configured idle window, or zero when the daemon is persistent.
func (s *Server) IdleExit() time.Duration {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	return s.idle.after
}

// NoteActivity records use of the daemon that did not arrive over this mux, and returns the function to call when that work finishes.
func (s *Server) NoteActivity() func() {
	s.idle.begin()
	return sync.OnceFunc(s.idle.end)
}

func (s *Server) idleMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !countsAsActivity(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		s.idle.begin()
		defer s.idle.end()
		next.ServeHTTP(w, r)
	})
}

func (t *idleTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inWork++
	t.last = time.Now()
}

func (t *idleTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inWork > 0 {
		t.inWork--
	}
	t.last = time.Now()
}

func (t *idleTracker) idleFor(now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.after <= 0 {
		return 0, false
	}
	if t.inWork > 0 {
		return 0, true
	}
	last := t.last
	if last.IsZero() {
		last = now
		t.last = now
	}
	return now.Sub(last), true
}

// WatchIdle blocks until the daemon has been idle for the configured window, or until ctx ends.
func (s *Server) WatchIdle(ctx context.Context) bool {
	after := s.IdleExit()
	if after <= 0 {
		return false
	}

	interval := max(50*time.Millisecond, after/4)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case now := <-ticker.C:
			idle, armed := s.idle.idleFor(now)
			if !armed {
				return false
			}
			if idle >= after {
				return true
			}
		}
	}
}
