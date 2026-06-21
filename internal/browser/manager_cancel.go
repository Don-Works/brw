package browser

import (
	"context"
	"strings"
	"sync"
)

// CancelResult reports the outcome of a brw_cancel request: how many
// in-flight long-running operations (plan / batch / wait loops) were signalled
// to stop for the given token. It is never an error to cancel a token with no
// matching operation in flight — Cancelled is simply 0.
type CancelResult struct {
	OK        bool   `json:"ok"`
	Token     string `json:"token"`
	Cancelled int    `json:"cancelled"`
	Message   string `json:"message,omitempty"`
}

// cancelAllToken is the wildcard token: cancelling it signals every in-flight
// cancellable operation regardless of which tab/token it registered under. This
// is the generic "stop everything" kill switch that replaces killing the daemon.
const cancelAllToken = "*"

// cancelEntry tracks one in-flight cancellable operation. Each plan/batch/wait
// loop registers an entry keyed by its token; cancelling the token closes the
// entry's context (so any context-aware CDP call unblocks) and flips the flag
// the cooperative loop checks between steps.
type cancelEntry struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	cancelled bool
}

func (e *cancelEntry) markCancelled() {
	e.mu.Lock()
	e.cancelled = true
	e.mu.Unlock()
	e.cancel()
}

// Cancelled reports whether this operation has been asked to stop. The plan and
// batch loops poll this between steps; the wait loop selects on entry.ctx.Done().
func (e *cancelEntry) Cancelled() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelled
}

// cancelRegistry maps a per-operation token to the set of in-flight entries
// running under it. Multiple operations can share a token (for example two
// plans targeting the same tab), so each token holds a set of live entries.
type cancelRegistry struct {
	mu      sync.Mutex
	entries map[string]map[*cancelEntry]struct{}
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{entries: map[string]map[*cancelEntry]struct{}{}}
}

// register derives a child context from parent, records it under token, and
// returns the entry plus a release func the caller MUST defer to deregister and
// release the context once the operation finishes (cancelled or not).
func (r *cancelRegistry) register(parent context.Context, token string) (*cancelEntry, func()) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	entry := &cancelEntry{ctx: ctx, cancel: cancel}

	r.mu.Lock()
	set, ok := r.entries[token]
	if !ok {
		set = map[*cancelEntry]struct{}{}
		r.entries[token] = set
	}
	set[entry] = struct{}{}
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		if set, ok := r.entries[token]; ok {
			delete(set, entry)
			if len(set) == 0 {
				delete(r.entries, token)
			}
		}
		r.mu.Unlock()
		cancel()
	}
	return entry, release
}

// cancel signals every entry registered under token (and every entry overall
// when token is the wildcard). Returns the number of operations signalled.
func (r *cancelRegistry) cancel(token string) int {
	r.mu.Lock()
	var targets []*cancelEntry
	if token == cancelAllToken {
		for _, set := range r.entries {
			for e := range set {
				targets = append(targets, e)
			}
		}
	} else if set, ok := r.entries[token]; ok {
		for e := range set {
			targets = append(targets, e)
		}
	}
	r.mu.Unlock()

	for _, e := range targets {
		e.markCancelled()
	}
	return len(targets)
}

// cancelToken derives the cancellation token for an operation from its context.
// Operations are keyed by tab id so a caller can cancel work targeting a
// specific tab without an explicit token; when no tab is set, the active-tab
// operations all share the same implicit "" key and the wildcard still reaches
// them. An explicit token always overrides.
func cancelToken(ctx context.Context, explicit string) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return t
	}
	return tabIDFromCtx(ctx)
}

// Cancel signals in-flight long-running operations (brw_plan, brw_batch,
// and their wait loops) to stop cooperatively. The token selects which
// operations to stop: an explicit token, the tab id (via WithTabID / tab_id),
// or "*" to stop everything. It is generic and never crashes a running op — the
// op returns a normal result reporting how many steps it completed plus
// cancelled=true. Cancelling a token with nothing in flight reports cancelled:0.
func (m *Manager) Cancel(ctx context.Context, token string) (CancelResult, error) {
	resolved := cancelToken(ctx, token)
	if resolved == "" {
		// No explicit token and no tab in context: fall back to the wildcard so
		// a bare brw_cancel still acts as the universal stop switch.
		resolved = cancelAllToken
	}
	n := m.cancels.cancel(resolved)
	res := CancelResult{OK: true, Token: resolved, Cancelled: n}
	if n == 0 {
		res.Message = "no in-flight cancellable operation matched token"
	}
	return res, nil
}
