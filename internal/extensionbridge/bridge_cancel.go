package extensionbridge

import (
	"context"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
)

const cancelAllToken = "*"

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

// Cancelled reports whether this operation has been asked to stop.
func (e *cancelEntry) Cancelled() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelled
}

type cancelRegistry struct {
	mu      sync.Mutex
	entries map[string]map[*cancelEntry]struct{}
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{entries: map[string]map[*cancelEntry]struct{}{}}
}

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

func cancelToken(ctx context.Context, explicit string) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return t
	}
	return browser.TabIDFromContext(ctx)
}

// Cancel signals in-flight long-running operations on the bridge to stop cooperatively.
func (b *Bridge) Cancel(ctx context.Context, token string) (browser.CancelResult, error) {
	resolved := cancelToken(ctx, token)
	if resolved == "" {
		resolved = cancelAllToken
	}
	n := b.cancels.cancel(resolved)
	res := browser.CancelResult{OK: true, Token: resolved, Cancelled: n}
	if n == 0 {
		res.Message = "no in-flight cancellable operation matched token"
	}
	return res, nil
}
