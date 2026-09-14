package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
)

// TestEventHubClassifiesTheSubscribedEvents drives each CDP event the single
// per-context subscription carries through ingest and checks what a waiter would
// see. A kind that stops being classified here stops waking the wait that reads
// it.
func TestEventHubClassifiesTheSubscribedEvents(t *testing.T) {
	tests := []struct {
		name       string
		event      any
		wantKind   pageEventKind
		wantScope  string
		wantDetail string
		wantURL    string
		wantStatus int64
		wantNone   bool
	}{
		{
			name:      "Page.loadEventFired",
			event:     &page.EventLoadEventFired{},
			wantKind:  eventLoad,
			wantScope: "tab-1",
		},
		{
			name:      "Page.frameNavigated for the main frame",
			event:     &page.EventFrameNavigated{Frame: &cdp.Frame{URL: "https://example.test/next"}},
			wantKind:  eventNavigated,
			wantScope: "tab-1",
			wantURL:   "https://example.test/next",
		},
		{
			name:     "Page.frameNavigated for a subframe is not a document change",
			event:    &page.EventFrameNavigated{Frame: &cdp.Frame{ParentID: "parent", URL: "https://ads.test/frame"}},
			wantNone: true,
		},
		{
			name:       "Page.javascriptDialogOpening",
			event:      &page.EventJavascriptDialogOpening{Type: page.DialogTypeConfirm, Message: "Delete this?", URL: "https://example.test/"},
			wantKind:   eventDialog,
			wantScope:  "tab-1",
			wantDetail: "confirm",
			wantURL:    "https://example.test/",
		},
		{
			name:       "Network.responseReceived",
			event:      &network.EventResponseReceived{Response: &network.Response{URL: "https://example.test/api", Status: 503}},
			wantKind:   eventResponse,
			wantScope:  "tab-1",
			wantURL:    "https://example.test/api",
			wantStatus: 503,
		},
		{
			name:     "Network.responseReceived with no response payload",
			event:    &network.EventResponseReceived{},
			wantNone: true,
		},
		{
			name:       "Runtime.consoleAPICalled",
			event:      &runtime.EventConsoleAPICalled{Type: runtime.APITypeError},
			wantKind:   eventConsole,
			wantScope:  "tab-1",
			wantDetail: "error",
		},
		{
			name:     "an event nothing waits on is dropped, not retained",
			event:    &page.EventDomContentEventFired{},
			wantNone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &eventHub{}
			sub, release := hub.subscribe("tab-1")
			defer release()

			hub.ingest("tab-1", tt.event)

			select {
			case got := <-sub:
				if tt.wantNone {
					t.Fatalf("event should have been dropped, got %+v", got)
				}
				if got.Kind != tt.wantKind {
					t.Fatalf("kind = %q, want %q", got.Kind, tt.wantKind)
				}
				if got.Scope != tt.wantScope {
					t.Fatalf("scope = %q, want %q", got.Scope, tt.wantScope)
				}
				if got.Detail != tt.wantDetail {
					t.Fatalf("detail = %q, want %q", got.Detail, tt.wantDetail)
				}
				if got.URL != tt.wantURL {
					t.Fatalf("url = %q, want %q", got.URL, tt.wantURL)
				}
				if got.Status != tt.wantStatus {
					t.Fatalf("status = %d, want %d", got.Status, tt.wantStatus)
				}
				if got.At.IsZero() {
					t.Fatal("event carries no timestamp, so no recency window can be applied to it")
				}
			case <-time.After(2 * time.Second):
				if !tt.wantNone {
					t.Fatal("subscriber was never woken")
				}
			}
		})
	}
}

// TestLoadStateFollowsNavigationAndLoad covers the state a readiness wait reads
// instead of asking the document. "observed" is what separates "this tab has not
// finished loading" from "brw attached after it already had".
func TestLoadStateFollowsNavigationAndLoad(t *testing.T) {
	hub := &eventHub{}

	if observed, loaded := hub.loadState("tab-1"); observed || loaded {
		t.Fatalf("a tab the hub has never seen must report observed=false loaded=false, got %v/%v", observed, loaded)
	}

	hub.ingest("tab-1", &page.EventFrameNavigated{Frame: &cdp.Frame{URL: "https://example.test/"}})
	if observed, loaded := hub.loadState("tab-1"); !observed || loaded {
		t.Fatalf("after a navigation want observed=true loaded=false, got %v/%v", observed, loaded)
	}

	hub.ingest("tab-1", &page.EventLoadEventFired{})
	if observed, loaded := hub.loadState("tab-1"); !observed || !loaded {
		t.Fatalf("after the load event want observed=true loaded=true, got %v/%v", observed, loaded)
	}

	// A subframe navigating must not make the tab look unloaded again.
	hub.ingest("tab-1", &page.EventFrameNavigated{Frame: &cdp.Frame{ParentID: "parent", URL: "https://ads.test/"}})
	if _, loaded := hub.loadState("tab-1"); !loaded {
		t.Fatal("a subframe navigation reset the tab's load state")
	}
}

// TestRetainedEventsAreCappedPerScope is the memory-leak guard. A busy page emits
// a Network.responseReceived per request; retaining them all would grow the ring
// with the page's traffic for the life of the tab.
func TestRetainedEventsAreCappedPerScope(t *testing.T) {
	hub := &eventHub{}
	const published = maxRetainedEventsPerScope * 3
	for i := range published {
		hub.ingest("tab-1", &network.EventResponseReceived{
			Response: &network.Response{URL: fmt.Sprintf("https://example.test/%d", i), Status: 200},
		})
	}

	hub.mu.Lock()
	scope := hub.scopes["tab-1"]
	retained := len(scope.ring)
	newest := scope.ring[len(scope.ring)-1].URL
	hub.mu.Unlock()

	if retained != maxRetainedEventsPerScope {
		t.Fatalf("retained %d events after publishing %d, want the %d cap", retained, published, maxRetainedEventsPerScope)
	}
	// The ring must keep the RECENT events; a wait asks about what just happened.
	if want := fmt.Sprintf("https://example.test/%d", published-1); newest != want {
		t.Fatalf("newest retained event = %q, want %q", newest, want)
	}
}

// TestRetainedResponseHeadersAreRedacted is the privacy guard. brw_network_capture
// refuses to hand back a live Authorization or Set-Cookie value; a retained CDP
// event must not be the way around that.
func TestRetainedResponseHeadersAreRedacted(t *testing.T) {
	hub := &eventHub{}
	sub, release := hub.subscribe("tab-1")
	defer release()

	hub.ingest("tab-1", &network.EventResponseReceived{Response: &network.Response{
		URL:    "https://example.test/session",
		Status: 200,
		Headers: network.Headers{
			"set-cookie":    "session=fixture-session-value-one; HttpOnly",
			"Authorization": "Bearer fixture-bearer-value-one",
			"content-type":  "application/json",
		},
	}})

	var got pageEvent
	select {
	case got = <-sub:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was never woken")
	}

	for _, name := range []string{"set-cookie", "Authorization"} {
		value, ok := got.Headers[name]
		if !ok {
			t.Fatalf("header %q was dropped entirely; the name is useful for debugging and must survive", name)
		}
		if value != "[redacted]" {
			t.Fatalf("header %q = %q, want it redacted", name, value)
		}
	}
	if got.Headers["content-type"] != "application/json" {
		t.Fatalf("content-type = %q, want it kept verbatim", got.Headers["content-type"])
	}
}

// TestPublishNeverBlocksOnASlowSubscriber: publish runs on chromedp's single
// event-dispatch goroutine. A blocking send there would stall every target's
// events behind the slowest waiter.
func TestPublishNeverBlocksOnASlowSubscriber(t *testing.T) {
	hub := &eventHub{}
	_, release := hub.subscribe("tab-1")
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range eventSubscriberBuffer * 4 {
			hub.publish("tab-1", pageEvent{Kind: eventConsole})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publish blocked on a subscriber that never read its channel")
	}
}

// TestScopeIsDroppedWhenItsContextEnds is the teardown guard: cancelling the
// context that owns a subscription must leave no scope, no retained event and no
// registered waiter behind.
func TestScopeIsDroppedWhenItsContextEnds(t *testing.T) {
	hub := &eventHub{}
	ctx, cancel := context.WithCancel(context.Background())

	hub.mu.Lock()
	hub.scopeLocked("tab-1").attached = true
	hub.mu.Unlock()
	hub.dropWhenDone("tab-1", ctx)

	_, release := hub.subscribe("tab-1")
	defer release()
	hub.ingest("tab-1", &page.EventLoadEventFired{})

	if hub.liveScopes() != 1 || hub.liveSubscribers() != 1 {
		t.Fatalf("before teardown: scopes=%d subscribers=%d, want 1/1", hub.liveScopes(), hub.liveSubscribers())
	}

	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.liveScopes() == 0 && hub.liveSubscribers() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after teardown: scopes=%d subscribers=%d, want 0/0", hub.liveScopes(), hub.liveSubscribers())
}

// TestReleasedSubscriptionLeavesNoScopeBehind: a wait against a scope nothing has
// attached to (no live tab context) conjures the scope. Releasing must take it
// with it, or every such wait leaks one map entry.
func TestReleasedSubscriptionLeavesNoScopeBehind(t *testing.T) {
	hub := &eventHub{}
	_, release := hub.subscribe(browserEventScope)
	if hub.liveScopes() != 1 {
		t.Fatalf("scopes = %d, want 1 while subscribed", hub.liveScopes())
	}
	release()
	if hub.liveScopes() != 0 {
		t.Fatalf("scopes = %d after release, want 0", hub.liveScopes())
	}
	// Release is idempotent.
	release()
}

// TestRecentReturnsOnlyMatchingEventsInsideTheWindow covers the "it already
// happened" half of a wait: brw answers a dialog the instant it opens, so the
// wait written after the click has to find it in the ring.
func TestRecentReturnsOnlyMatchingEventsInsideTheWindow(t *testing.T) {
	hub := &eventHub{}
	now := time.Now()
	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "old", At: now.Add(-time.Hour)})
	hub.publish("tab-1", pageEvent{Kind: eventConsole, At: now})
	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "fresh", At: now})

	got := hub.recent("tab-1", eventDialog, now.Add(-time.Minute))
	if len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("recent = %+v, want only the dialog inside the window", got)
	}
	if got := hub.recent("tab-unknown", eventDialog, now.Add(-time.Minute)); got != nil {
		t.Fatalf("recent on an unknown scope = %+v, want nil", got)
	}
}

// TestAwaitPrearmedSettleAbandonsTheScriptOnNavigation proves the settle window's
// navigation branch now comes from the event stream. The in-page promise it is
// waiting on lives in the execution context the navigation is destroying, so
// without the stream the settle waits for a reply that will never arrive.
func TestAwaitPrearmedSettleAbandonsTheScriptOnNavigation(t *testing.T) {
	tests := []struct {
		name     string
		event    *pageEvent
		want     string
		awaitFor time.Duration
	}{
		{
			name:     "the in-page settle resolving first wins",
			awaitFor: 10 * time.Millisecond,
			want:     settleSourceScript,
		},
		{
			name:     "a navigation abandons an in-page settle that cannot answer",
			event:    &pageEvent{Kind: eventNavigated},
			awaitFor: time.Hour,
			want:     settleSourceNavigation,
		},
		{
			name:     "an unrelated event does not end the settle window",
			event:    &pageEvent{Kind: eventConsole},
			awaitFor: 40 * time.Millisecond,
			want:     settleSourceScript,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &eventHub{}
			sub, release := hub.subscribe("tab-1")
			defer release()

			started := make(chan struct{})
			done := make(chan string, 1)
			go func() {
				done <- awaitPrearmedSettle(sub, 5*time.Second, func() {
					close(started)
					time.Sleep(tt.awaitFor)
				})
			}()
			<-started
			if tt.event != nil {
				hub.publish("tab-1", *tt.event)
			}

			select {
			case got := <-done:
				if got != tt.want {
					t.Fatalf("settle source = %q, want %q", got, tt.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("settle never returned; want source %q", tt.want)
			}
		})
	}
}

// TestAwaitPrearmedSettleWithoutASubscriptionStillResolves: a context with no
// event scope (a tab brw has not bound) must degrade to the in-page settle
// rather than hang or skip the window.
func TestAwaitPrearmedSettleWithoutASubscriptionStillResolves(t *testing.T) {
	awaited := false
	got := awaitPrearmedSettle(nil, time.Second, func() { awaited = true })
	if !awaited {
		t.Fatal("the in-page settle was skipped when no subscription was available")
	}
	if got != settleSourceScript {
		t.Fatalf("settle source = %q, want %q", got, settleSourceScript)
	}
}

// TestAwaitEventOutcomes covers every way a wait on the shared stream can end.
// The "tab closed" case is the one that is easy to get wrong: the subscription
// dies with the tab, so a wait that only watched its own deadline would sit for
// the full timeout and then report the wrong reason.
func TestAwaitEventOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		publish    []pageEvent
		cancelCall bool
		closeTab   bool
		timeout    time.Duration
		wantErr    string
		wantWakes  int
	}{
		{
			name:      "the matching event resolves the wait",
			publish:   []pageEvent{{Kind: eventDialog}},
			timeout:   5 * time.Second,
			wantWakes: 1,
		},
		{
			name:      "events that do not match keep the wait open",
			publish:   []pageEvent{{Kind: eventConsole}, {Kind: eventResponse}, {Kind: eventDialog}},
			timeout:   5 * time.Second,
			wantWakes: 3,
		},
		{
			name:       "the caller cancelling reports a cancellation",
			cancelCall: true,
			timeout:    5 * time.Second,
			wantErr:    `wait for "dialog" cancelled`,
		},
		{
			name:     "the tab going away reports that, not a timeout",
			closeTab: true,
			timeout:  5 * time.Second,
			wantErr:  `wait for "dialog" ended: the tab closed`,
		},
		{
			name:    "nothing arriving reports a timeout",
			timeout: 150 * time.Millisecond,
			wantErr: `timed out waiting for "dialog"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &eventHub{}
			sub, release := hub.subscribe("tab-1")
			defer release()

			ctx, cancelCall := context.WithCancel(context.Background())
			defer cancelCall()
			tabCtx, closeTab := context.WithCancel(context.Background())
			defer closeTab()

			done := make(chan struct {
				outcome WaitOutcome
				err     error
			}, 1)
			go func() {
				outcome, err := awaitEvent(ctx, tabCtx, sub, tt.timeout, "dialog", func(ev pageEvent) bool {
					return ev.Kind == eventDialog
				})
				done <- struct {
					outcome WaitOutcome
					err     error
				}{outcome, err}
			}()

			for _, ev := range tt.publish {
				hub.publish("tab-1", ev)
			}
			if tt.cancelCall {
				cancelCall()
			}
			if tt.closeTab {
				closeTab()
			}

			select {
			case got := <-done:
				if tt.wantErr == "" {
					if got.err != nil {
						t.Fatalf("wait failed: %v", got.err)
					}
					if got.outcome.ResolvedBy != WaitResolvedByEvent {
						t.Fatalf("resolved_by = %q, want %q", got.outcome.ResolvedBy, WaitResolvedByEvent)
					}
					if got.outcome.Wakeups != tt.wantWakes {
						t.Fatalf("wakeups = %d, want %d", got.outcome.Wakeups, tt.wantWakes)
					}
					return
				}
				if got.err == nil || got.err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", got.err, tt.wantErr)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("wait never returned; want %q", tt.wantErr)
			}
		})
	}
}
