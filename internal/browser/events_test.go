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

// everyKind is what a test subscriber asks for when the point of the test is
// something other than the kind filter.
var everyKind = []pageEventKind{eventLoad, eventNavigated, eventDialog, eventDownload}

// attachScope models what attachTab does before any event can arrive: the scope
// exists and something is responsible for dropping it. publish and the load-state
// marks deliberately refuse to create a scope, so a test that ingests without
// this would be exercising the post-teardown path instead.
func attachScope(t *testing.T, hub *eventHub, name string) {
	t.Helper()
	hub.mu.Lock()
	hub.scopeLocked(name).attached = true
	hub.mu.Unlock()
}

// TestEventHubClassifiesTheSubscribedEvents drives each CDP event the single
// per-context subscription carries through ingest and checks what a waiter would
// see. A kind that stops being classified here stops waking the wait that reads
// it — and a kind that starts being carried when nothing consumes it takes a slot
// in every waiter's finite queue.
func TestEventHubClassifiesTheSubscribedEvents(t *testing.T) {
	tests := []struct {
		name       string
		event      any
		wantKind   pageEventKind
		wantScope  string
		wantDetail string
		wantURL    string
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
			name: "Network.responseReceived is not carried: no wait reads it",
			event: &network.EventResponseReceived{Response: &network.Response{
				URL:    "https://example.test/api",
				Status: 503,
				Headers: network.Headers{
					"set-cookie":   "session=fixture-session-value-one",
					"content-type": "application/json",
				},
			}},
			wantNone: true,
		},
		{
			name:     "Runtime.consoleAPICalled is not carried: no wait reads it",
			event:    &runtime.EventConsoleAPICalled{Type: runtime.APITypeError},
			wantNone: true,
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
			attachScope(t, hub, "tab-1")
			sub, release := hub.subscribe(everyKind, "tab-1")
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
				if got.At.IsZero() {
					t.Fatal("event carries no timestamp, so no recency window can be applied to it")
				}
			case <-time.After(2 * time.Second):
				if !tt.wantNone {
					t.Fatal("subscriber was never woken")
				}
			}

			// What the hub declines to carry must not be retained either: the ring
			// is the other half of the memory and privacy bound.
			if tt.wantNone {
				hub.mu.Lock()
				retained := 0
				if sc := hub.scopes["tab-1"]; sc != nil {
					for _, ring := range sc.rings {
						retained += len(ring)
					}
				}
				hub.mu.Unlock()
				if retained != 0 {
					t.Fatalf("retained %d events of a kind nothing consumes", retained)
				}
			}
		})
	}
}

// TestASubscriberOnlyReceivesTheKindsItAskedFor is the queue-crowding guard. The
// per-waiter queue is finite and a full one drops what lands next, so a waiter
// also handed kinds it does not care about can have the event it is waiting for
// pushed out by ordinary page traffic. Publishing the burst BEFORE the wait looks
// is the real shape of it: the events are already queued when it does.
func TestASubscriberOnlyReceivesTheKindsItAskedFor(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")
	sub, release := hub.subscribe([]pageEventKind{eventDialog}, "tab-1")
	defer release()

	for range eventSubscriberBuffer * 2 {
		hub.publish("tab-1", pageEvent{Kind: eventLoad})
	}
	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "Delete this?"})

	outcome, err := awaitEvent(context.Background(), context.Background(), sub, 2*time.Second, "dialog", func(ev pageEvent) bool {
		return ev.Kind == eventDialog
	})
	if err != nil {
		t.Fatalf("dialog wait: %v — a burst of another kind pushed the dialog out of the queue", err)
	}
	if outcome.Wakeups != 1 {
		t.Fatalf("wakeups = %d, want 1: the wait was woken by events it never asked for", outcome.Wakeups)
	}
}

// TestLoadStateFollowsNavigationAndLoad covers the state a readiness wait reads
// instead of asking the document. "observed" is what separates "this tab has not
// finished loading" from "brw attached after it already had".
func TestLoadStateFollowsNavigationAndLoad(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")

	if observed, loaded := hub.loadState("tab-other"); observed || loaded {
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

// TestRetainedEventsAreCappedPerKind is the memory-leak guard: retention must not
// grow with how long a tab lives.
func TestRetainedEventsAreCappedPerKind(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")
	const published = maxRetainedEventsPerKind * 3
	for i := range published {
		hub.publish("tab-1", pageEvent{Kind: eventNavigated, URL: fmt.Sprintf("https://example.test/%d", i)})
	}

	hub.mu.Lock()
	ring := hub.scopes["tab-1"].rings[eventNavigated]
	retained := len(ring)
	newest := ring[len(ring)-1].URL
	hub.mu.Unlock()

	if retained != maxRetainedEventsPerKind {
		t.Fatalf("retained %d events after publishing %d, want the %d cap", retained, published, maxRetainedEventsPerKind)
	}
	// The ring must keep the RECENT events; a wait asks about what just happened.
	if want := fmt.Sprintf("https://example.test/%d", published-1); newest != want {
		t.Fatalf("newest retained event = %q, want %q", newest, want)
	}
}

// TestOneKindsTrafficDoesNotEvictAnother is the other half of that guard and the
// reason retention is keyed by kind. brw answers a dialog the instant it opens,
// so the wait written after the click looks the dialog up in the ring; one shared
// ring makes that lookup a race against whatever else the page did in between.
func TestOneKindsTrafficDoesNotEvictAnother(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")
	since := time.Now().Add(-time.Minute)

	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "Delete this?"})
	for i := range maxRetainedEventsPerKind * 4 {
		hub.publish("tab-1", pageEvent{Kind: eventNavigated, URL: fmt.Sprintf("https://example.test/%d", i)})
		hub.publish("tab-1", pageEvent{Kind: eventLoad})
	}

	got := hub.recent("tab-1", eventDialog, since)
	if len(got) != 1 || got[0].Text != "Delete this?" {
		t.Fatalf("recent dialogs = %+v, want the one dialog: other traffic evicted it", got)
	}
}

// TestPublishNeverBlocksOnASlowSubscriber: publish runs on chromedp's single
// event-dispatch goroutine. A blocking send there would stall every target's
// events behind the slowest waiter.
func TestPublishNeverBlocksOnASlowSubscriber(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")
	_, release := hub.subscribe([]pageEventKind{eventDialog}, "tab-1")
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range eventSubscriberBuffer * 4 {
			hub.publish("tab-1", pageEvent{Kind: eventDialog})
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
// registered waiter behind — and must keep it that way. chromedp tests each
// listener's context per event, so an event that passed that test can still
// arrive after the scope is gone.
func TestScopeIsDroppedWhenItsContextEnds(t *testing.T) {
	hub := &eventHub{}
	ctx, cancel := context.WithCancel(context.Background())

	attachScope(t, hub, "tab-1")
	hub.dropWhenDone("tab-1", ctx)

	_, release := hub.subscribe(everyKind, "tab-1")
	defer release()
	hub.ingest("tab-1", &page.EventLoadEventFired{})

	if hub.liveScopes() != 1 || hub.liveSubscribers() != 1 {
		t.Fatalf("before teardown: scopes=%d subscribers=%d, want 1/1", hub.liveScopes(), hub.liveSubscribers())
	}

	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for hub.liveScopes() != 0 || hub.liveSubscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after teardown: scopes=%d subscribers=%d, want 0/0", hub.liveScopes(), hub.liveSubscribers())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A late event must not bring the scope back. A resurrected scope has no
	// context left to drop it, so it and everything it retains would then live for
	// the daemon's lifetime.
	hub.ingest("tab-1", &page.EventLoadEventFired{})
	hub.ingest("tab-1", &page.EventFrameNavigated{Frame: &cdp.Frame{URL: "https://example.test/"}})
	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "after teardown"})
	if hub.liveScopes() != 0 {
		t.Fatalf("an event delivered after teardown resurrected the scope: scopes=%d, want 0", hub.liveScopes())
	}
	if observed, loaded := hub.loadState("tab-1"); observed || loaded {
		t.Fatalf("load state survived teardown: observed=%v loaded=%v", observed, loaded)
	}
}

// TestReleasedSubscriptionLeavesNoScopeBehind: a wait against a scope nothing has
// attached to (no live tab context) conjures the scope. Releasing must take it
// with it, or every such wait leaks one map entry — including whatever landed in
// it meanwhile, which nothing can read once the last subscriber is gone.
func TestReleasedSubscriptionLeavesNoScopeBehind(t *testing.T) {
	tests := []struct {
		name    string
		publish []pageEvent
	}{
		{name: "nothing arrived while it was subscribed"},
		{
			name:    "a download landed while it was subscribed",
			publish: []pageEvent{{Kind: eventDownload, ID: "guid-1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &eventHub{}
			_, release := hub.subscribe([]pageEventKind{eventDownload}, browserEventScope)
			if hub.liveScopes() != 1 {
				t.Fatalf("scopes = %d, want 1 while subscribed", hub.liveScopes())
			}
			for _, ev := range tt.publish {
				hub.publish(browserEventScope, ev)
			}
			release()
			if hub.liveScopes() != 0 {
				t.Fatalf("scopes = %d after release, want 0", hub.liveScopes())
			}
			// Release is idempotent.
			release()
		})
	}
}

// TestRecentReturnsOnlyMatchingEventsInsideTheWindow covers the "it already
// happened" half of a wait: brw answers a dialog the instant it opens, so the
// wait written after the click has to find it in the ring.
func TestRecentReturnsOnlyMatchingEventsInsideTheWindow(t *testing.T) {
	hub := &eventHub{}
	attachScope(t, hub, "tab-1")
	now := time.Now()
	hub.publish("tab-1", pageEvent{Kind: eventDialog, Text: "old", At: now.Add(-time.Hour)})
	hub.publish("tab-1", pageEvent{Kind: eventLoad, At: now})
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
// navigation branch comes from the event stream. The in-page promise it is
// waiting on lives in the execution context the navigation is destroying, so
// without the stream the settle waits for a reply that will never arrive.
//
// It also pins what happens to the abandoned await, in both directions. It must
// NOT be cancelled when the window ends: that kills a CDP command in flight on a
// live tab just as the next action is issued, which cost an inline upload its
// file bytes on the very next submit. It must end with the tab, which is what
// keeps one abandoned wait per action from outliving anything.
func TestAwaitPrearmedSettleAbandonsTheScriptOnNavigation(t *testing.T) {
	tests := []struct {
		name      string
		subscribe bool
		event     *pageEvent
		awaitFor  time.Duration
		settleCap time.Duration
	}{
		{
			name:      "the in-page settle resolving first wins",
			subscribe: true,
			awaitFor:  10 * time.Millisecond,
			settleCap: 5 * time.Second,
		},
		{
			name:      "a navigation abandons an in-page settle that cannot answer",
			subscribe: true,
			event:     &pageEvent{Kind: eventNavigated},
			awaitFor:  time.Hour,
			settleCap: 5 * time.Second,
		},
		{
			name:      "an unrelated event does not end the settle window",
			subscribe: true,
			event:     &pageEvent{Kind: eventDialog},
			awaitFor:  40 * time.Millisecond,
			settleCap: 5 * time.Second,
		},
		{
			name:      "a renderer that never replies is bounded by the cap",
			subscribe: true,
			awaitFor:  time.Hour,
			settleCap: 50 * time.Millisecond,
		},
		{
			name:      "no subscription still runs the in-page settle",
			awaitFor:  10 * time.Millisecond,
			settleCap: time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := &eventHub{}
			attachScope(t, hub, "tab-1")
			var sub <-chan pageEvent
			if tt.subscribe {
				stream, release := hub.subscribe([]pageEventKind{eventNavigated}, "tab-1")
				defer release()
				sub = stream
			}

			tabCtx, closeTab := context.WithCancel(context.Background())
			defer closeTab()

			started := make(chan struct{})
			awaited := make(chan context.Context, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				awaitPrearmedSettle(tabCtx, sub, tt.settleCap, func(ctx context.Context) {
					awaited <- ctx
					close(started)
					select {
					case <-ctx.Done():
					case <-time.After(tt.awaitFor):
					}
				})
			}()
			<-started
			if tt.event != nil {
				hub.publish("tab-1", *tt.event)
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the settle window never ended")
			}

			// The abandoned evaluate is still a live CDP command on a live tab, and
			// the caller is about to issue its post-action snapshot against that tab.
			awaitCtx := <-awaited
			if err := awaitCtx.Err(); err != nil {
				t.Fatalf("the in-page settle was cancelled when the window ended: %v", err)
			}
			// It ends with the tab, so an abandoned wait cannot outlive it.
			closeTab()
			select {
			case <-awaitCtx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("the abandoned in-page settle outlived the tab context it was issued on")
			}
		})
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
			publish:   []pageEvent{{Kind: eventLoad}, {Kind: eventLoad}, {Kind: eventDialog}},
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
			attachScope(t, hub, "tab-1")
			sub, release := hub.subscribe([]pageEventKind{eventLoad, eventDialog}, "tab-1")
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
