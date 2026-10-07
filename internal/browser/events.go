package browser

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type eventHub struct {
	mu      sync.Mutex
	scopes  map[string]*eventScope
	nextSub uint64
}

const browserEventScope = "*browser*"

const (
	maxRetainedEventsPerKind = 32

	eventSubscriberBuffer = 16

	maxEventTextBytes = 400
)

type pageEventKind string

const (
	eventLoad      pageEventKind = "load"
	eventNavigated pageEventKind = "navigated"
	eventDialog    pageEventKind = "dialog"
	eventDownload  pageEventKind = "download"
)

type pageEvent struct {
	Kind   pageEventKind
	Scope  string
	At     time.Time
	URL    string
	Detail string // dialog type or download state
	Text   string // dialog message, clipped
	ID     string // download guid
}

type eventSubscriber struct {
	ch    chan pageEvent
	kinds map[pageEventKind]bool
}

type eventScope struct {
	rings map[pageEventKind][]pageEvent
	subs  map[uint64]*eventSubscriber

	attached bool

	observed bool
	loaded   bool
}

func (h *eventHub) scopeLocked(name string) *eventScope {
	if h.scopes == nil {
		h.scopes = make(map[string]*eventScope)
	}
	scope, ok := h.scopes[name]
	if !ok {
		scope = &eventScope{
			rings: make(map[pageEventKind][]pageEvent),
			subs:  make(map[uint64]*eventSubscriber),
		}
		h.scopes[name] = scope
	}
	return scope
}

func (h *eventHub) attachTab(tabID string, tabCtx context.Context, raw func(ev any)) {
	h.mu.Lock()
	scope := h.scopeLocked(tabID)
	if scope.attached {
		h.mu.Unlock()
		return
	}
	scope.attached = true
	h.mu.Unlock()

	chromedp.ListenTarget(tabCtx, func(ev any) {
		if raw != nil {
			raw(ev)
		}
		h.ingest(tabID, ev)
	})
	h.dropWhenDone(tabID, tabCtx)
}

func (h *eventHub) attachBrowser(browserCtx context.Context, raw func(ev any)) {
	h.mu.Lock()
	scope := h.scopeLocked(browserEventScope)
	if scope.attached {
		h.mu.Unlock()
		return
	}
	scope.attached = true
	h.mu.Unlock()

	chromedp.ListenBrowser(browserCtx, func(ev any) {
		if raw != nil {
			raw(ev)
		}
		h.ingest(browserEventScope, ev)
	})
	h.dropWhenDone(browserEventScope, browserCtx)
}

func (h *eventHub) dropWhenDone(name string, ctx context.Context) {
	context.AfterFunc(ctx, func() { h.closeScope(name) })
}

func (h *eventHub) ingest(scope string, ev any) {
	switch e := ev.(type) {
	case *page.EventLoadEventFired:
		h.markLoaded(scope)
		h.publish(scope, pageEvent{Kind: eventLoad})
	case *page.EventFrameNavigated:

		if e.Frame == nil || e.Frame.ParentID != "" {
			return
		}
		h.markNavigated(scope)
		h.publish(scope, pageEvent{Kind: eventNavigated, URL: clipEventText(e.Frame.URL)})
	case *page.EventJavascriptDialogOpening:
		h.publish(scope, pageEvent{
			Kind:   eventDialog,
			Detail: string(e.Type),
			Text:   clipEventText(e.Message),
			URL:    clipEventText(e.URL),
		})
	}

}

func (h *eventHub) publish(scope string, ev pageEvent) {
	ev.Scope = scope
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopes[scope]
	if sc == nil {

		return
	}
	ring := append(sc.rings[ev.Kind], ev)
	if len(ring) > maxRetainedEventsPerKind {
		ring = slices.Delete(ring, 0, len(ring)-maxRetainedEventsPerKind)
	}
	sc.rings[ev.Kind] = ring
	for _, sub := range sc.subs {
		if !sub.kinds[ev.Kind] {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

func (h *eventHub) subscribe(kinds []pageEventKind, scopes ...string) (<-chan pageEvent, func()) {
	wanted := make(map[pageEventKind]bool, len(kinds))
	for _, kind := range kinds {
		wanted[kind] = true
	}
	ch := make(chan pageEvent, eventSubscriberBuffer)
	ids := make(map[string]uint64, len(scopes))
	h.mu.Lock()
	for _, name := range scopes {
		if _, dup := ids[name]; dup {
			continue
		}
		sc := h.scopeLocked(name)
		h.nextSub++
		sc.subs[h.nextSub] = &eventSubscriber{ch: ch, kinds: wanted}
		ids[name] = h.nextSub
	}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			for name, id := range ids {
				sc := h.scopes[name]
				if sc == nil {
					continue
				}
				delete(sc.subs, id)

				if !sc.attached && len(sc.subs) == 0 {
					delete(h.scopes, name)
				}
			}
		})
	}
}

func (h *eventHub) recent(scope string, kind pageEventKind, since time.Time) []pageEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopes[scope]
	if sc == nil {
		return nil
	}
	var out []pageEvent
	for _, ev := range sc.rings[kind] {
		if !ev.At.Before(since) {
			out = append(out, ev)
		}
	}
	return out
}

func (h *eventHub) loadState(tabID string) (observed, loaded bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopes[tabID]
	if sc == nil {
		return false, false
	}
	return sc.observed, sc.loaded
}

func (h *eventHub) markLoaded(scope string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sc := h.scopes[scope]; sc != nil {
		sc.observed = true
		sc.loaded = true
	}
}

func (h *eventHub) markNavigated(scope string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sc := h.scopes[scope]; sc != nil {
		sc.observed = true
		sc.loaded = false
	}
}

func (h *eventHub) closeScope(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.scopes, name)
}

func (h *eventHub) liveScopes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.scopes)
}

func (h *eventHub) liveSubscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, sc := range h.scopes {
		total += len(sc.subs)
	}
	return total
}

func clipEventText(s string) string {
	if len(s) <= maxEventTextBytes {
		return s
	}
	return s[:maxEventTextBytes]
}
