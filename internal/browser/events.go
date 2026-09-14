package browser

import (
	"context"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// eventHub turns the CDP event stream into the source of truth for waiting.
//
// brw used to learn that something had happened by asking again: a download wait
// re-read the registry every 50ms, and a readiness wait armed a fresh in-page
// promise per call. Each ask is a round trip and a latency floor, and on the
// extension bridge also a message-port wake.
//
// One subscription is installed per chromedp context — not per wait — carrying
// Page.loadEventFired, Page.frameNavigated, Page.javascriptDialogOpening,
// Network.responseReceived, Runtime.consoleAPICalled and
// Browser.downloadProgress. Every waiter reads that shared stream. The
// subscription is scoped to the context that created it and the scope is dropped
// when the context ends, so a closed tab leaves no listener, no ring and no
// registered waiter behind.
type eventHub struct {
	mu      sync.Mutex
	scopes  map[string]*eventScope
	nextSub uint64
}

// browserEventScope is the key for browser-domain events, which belong to the
// connection rather than to any one tab. Chrome delivers Browser.downloadProgress
// on the browser connection AND on the page session; routing both deliveries to
// one scope means a wait subscribes once and cannot miss whichever arrives.
// A Chrome target id is uppercase hex, so the '*' cannot collide with a tab id.
const browserEventScope = "*browser*"

const (
	// maxRetainedEventsPerScope bounds the per-scope ring. A wait reads the ring
	// to answer "did this already happen just now?"; anything older is answered
	// by the live stream. Network.responseReceived is why this is a ring and not
	// a slice: unbounded retention would grow with the page's traffic for the
	// life of the tab, which is a memory leak and a standing privacy surface.
	maxRetainedEventsPerScope = 128

	// eventSubscriberBuffer is the per-waiter queue depth. Sends are
	// non-blocking: a woken waiter re-reads authoritative state, so a dropped
	// duplicate wake-up costs nothing, while a blocking send would stall
	// chromedp's single event-dispatch goroutine behind the slowest waiter.
	eventSubscriberBuffer = 16

	// maxEventTextBytes clips retained dialog text and URLs.
	maxEventTextBytes = 400

	// maxRetainedResponseHeaders bounds how much of a response header block is
	// retained per event.
	maxRetainedResponseHeaders = 16
)

// pageEventKind names one signal the wait and settle paths consume.
type pageEventKind string

const (
	eventLoad      pageEventKind = "load"
	eventNavigated pageEventKind = "navigated"
	eventDialog    pageEventKind = "dialog"
	eventResponse  pageEventKind = "response"
	eventConsole   pageEventKind = "console"
	eventDownload  pageEventKind = "download"
)

// pageEvent is one normalised CDP event. It carries only what a wait needs to
// decide; the authoritative state (the download registry, the dialog ring) is
// re-read by the waiter after it wakes.
type pageEvent struct {
	Kind    pageEventKind
	Scope   string
	At      time.Time
	URL     string
	Detail  string // dialog type, console level, or download state
	Text    string // dialog message, clipped
	ID      string // download guid
	Status  int64  // response status
	Headers map[string]string
}

type eventScope struct {
	ring     []pageEvent
	subs     map[uint64]chan pageEvent
	attached bool
	// observed is true once the hub has seen a lifecycle event for this scope.
	// Without it a wait cannot tell "this tab has not loaded" from "brw attached
	// after the load already fired", and only the second case may fall back to
	// reading the document.
	observed bool
	loaded   bool
}

func (h *eventHub) scopeLocked(name string) *eventScope {
	if h.scopes == nil {
		h.scopes = make(map[string]*eventScope)
	}
	scope, ok := h.scopes[name]
	if !ok {
		scope = &eventScope{subs: make(map[uint64]chan pageEvent)}
		h.scopes[name] = scope
	}
	return scope
}

// attachTab installs the single per-tab subscription. raw runs FIRST on every
// event: the handlers it fans out to (the download registry, the dialog ring,
// the console buffer) own the state a woken waiter re-reads, so publishing
// before they ran would hand the waiter the old state with no further event
// coming to correct it.
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

// attachBrowser installs the single browser-connection subscription. Browser
// domain events (downloads) are not tab-scoped.
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

// dropWhenDone ties a scope's lifetime to the context that owns it. This is the
// whole of the teardown contract: no wait, no ring and no retained event
// outlives the context it was scoped to.
func (h *eventHub) dropWhenDone(name string, ctx context.Context) {
	go func() {
		<-ctx.Done()
		h.closeScope(name)
	}()
}

// ingest normalises one CDP event and publishes it. Events the wait and settle
// paths do not consume are dropped here rather than retained.
func (h *eventHub) ingest(scope string, ev any) {
	switch e := ev.(type) {
	case *page.EventLoadEventFired:
		h.markLoaded(scope)
		h.publish(scope, pageEvent{Kind: eventLoad})
	case *page.EventFrameNavigated:
		// Subframes do not replace the tab's document, so they must not reset
		// the load state a "load" wait reads.
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
	case *network.EventResponseReceived:
		if e.Response == nil {
			return
		}
		h.publish(scope, pageEvent{
			Kind:    eventResponse,
			URL:     clipEventText(e.Response.URL),
			Status:  e.Response.Status,
			Headers: redactedResponseHeaders(e.Response.Headers),
		})
	case *runtime.EventConsoleAPICalled:
		h.publish(scope, pageEvent{Kind: eventConsole, Detail: string(e.Type)})
	}
	// Browser.downloadProgress is deliberately absent: it is published by
	// Manager.handleDownloadEventForTab, which is wired into this same
	// subscription and writes the download registry first. Publishing it here as
	// well would wake a waiter before the registry it re-reads had been updated,
	// with no later event to correct it.
}

func (h *eventHub) publish(scope string, ev pageEvent) {
	ev.Scope = scope
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopeLocked(scope)
	sc.ring = append(sc.ring, ev)
	if len(sc.ring) > maxRetainedEventsPerScope {
		sc.ring = append(sc.ring[:0], sc.ring[len(sc.ring)-maxRetainedEventsPerScope:]...)
	}
	for _, ch := range sc.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe returns one channel fed by every named scope, plus a release that
// must be called when the wait ends. Subscribing is what a wait does instead of
// opening its own poll loop.
func (h *eventHub) subscribe(scopes ...string) (<-chan pageEvent, func()) {
	ch := make(chan pageEvent, eventSubscriberBuffer)
	ids := make(map[string]uint64, len(scopes))
	h.mu.Lock()
	for _, name := range scopes {
		if _, dup := ids[name]; dup {
			continue
		}
		sc := h.scopeLocked(name)
		h.nextSub++
		sc.subs[h.nextSub] = ch
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
				// A scope conjured by a subscribe against a context that never
				// attached has nothing to tear it down later; drop it here.
				if !sc.attached && len(sc.subs) == 0 && len(sc.ring) == 0 {
					delete(h.scopes, name)
				}
			}
		})
	}
}

// recent returns retained events of one kind that landed at or after since,
// oldest first. It answers the "it already happened" half of a wait: a dialog is
// answered and gone, and a small download finishes, before the wait that follows
// the click is even issued.
func (h *eventHub) recent(scope string, kind pageEventKind, since time.Time) []pageEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopes[scope]
	if sc == nil {
		return nil
	}
	var out []pageEvent
	for _, ev := range sc.ring {
		if ev.Kind == kind && !ev.At.Before(since) {
			out = append(out, ev)
		}
	}
	return out
}

// loadState reports whether the hub has seen any lifecycle event for a tab, and
// whether the current document has fired its load event.
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
	sc := h.scopeLocked(scope)
	sc.observed = true
	sc.loaded = true
}

func (h *eventHub) markNavigated(scope string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sc := h.scopeLocked(scope)
	sc.observed = true
	sc.loaded = false
}

// closeScope drops everything the hub holds for one context: the retained ring
// and every registered waiter. Waiters are not signalled here because a wait is
// always bounded by a context descended from the one that just died.
func (h *eventHub) closeScope(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.scopes, name)
}

// liveScopes and liveSubscribers exist for the teardown guard: a context that
// has been cancelled must leave neither behind.
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

// redactedResponseHeaders bounds and redacts a retained response header block.
// brw_network_capture already refuses to hand back a live Authorization or
// Set-Cookie value; a retained CDP event must not become the side door around
// that, so it runs through the same denylist before anything is kept.
func redactedResponseHeaders(headers network.Headers) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, min(len(headers), maxRetainedResponseHeaders))
	for name, value := range headers {
		if len(out) >= maxRetainedResponseHeaders {
			break
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		out[name] = clipEventText(text)
	}
	return snapshot.RedactSensitiveHeaders(out)
}

func clipEventText(s string) string {
	if len(s) <= maxEventTextBytes {
		return s
	}
	return s[:maxEventTextBytes]
}
