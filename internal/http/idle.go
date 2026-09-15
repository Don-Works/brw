package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Idle shutdown for a daemon that is not meant to be permanent.
//
// brw's default daemon is persistent: it is a background service, it holds a
// browser, and it should still be there tomorrow. But a daemon started for one
// job — a scheduled run, a CI step, a `brwd --remote` attached to a browser
// someone else launched — has no owner once that job ends, and it keeps a Chrome
// and a loopback port for as long as the machine is up. The MCP stdio server
// already has --mcp-idle-exit for exactly this; the HTTP daemon had nothing.
//
// Off by default, because the wrong default here is an agent coming back to a
// daemon that quietly exited.

// idleExemptPrefixes are the request paths that do NOT count as work.
//
// A health poll is the obvious one: a supervisor checking every thirty seconds
// would otherwise keep an abandoned daemon alive forever, which is precisely the
// state this exists to end. Everything else — every /api/ call, the dashboard, a
// session stream — is somebody using the daemon.
//
// It is a table rather than a condition at the call site because the answer has
// to be decidable for every route the daemon serves;
// TestEveryRouteIsClassifiedForIdleActivity enumerates them against it.
var idleExemptPrefixes = map[string]string{
	"/health": "a supervisor's liveness poll is not work; counting it would keep an abandoned daemon alive forever",
}

// countsAsActivity reports whether a request should postpone an idle exit.
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

// SetIdleExit arms an idle shutdown after d without a request that counts as
// work. Zero (the default) disables it and keeps the daemon persistent.
func (s *Server) SetIdleExit(d time.Duration) {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	s.idle.after = d
	s.idle.last = time.Now()
}

// IdleExit reports the configured idle window, or zero when the daemon is
// persistent.
func (s *Server) IdleExit() time.Duration {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	return s.idle.after
}

// idleMiddleware records that somebody used the daemon. A request is counted on
// the way in AND on the way out: a single long call — a recipe run, a wait —
// must not look like silence just because it has not finished yet.
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

// idleFor reports how long the daemon has been doing nothing. A call still in
// flight is never idle, however long it has been running.
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

// WatchIdle blocks until the daemon has been idle for the configured window, or
// until ctx ends. It returns true only when the idle window elapsed, so the
// caller can tell a deliberate shutdown from a cancelled one and say which in
// its log.
//
// It returns false immediately when no idle window is configured, which is the
// default: a persistent daemon has nothing to watch for.
func (s *Server) WatchIdle(ctx context.Context) bool {
	after := s.IdleExit()
	if after <= 0 {
		return false
	}
	// Check several times per window so the daemon exits near its deadline
	// rather than up to a whole window late.
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
