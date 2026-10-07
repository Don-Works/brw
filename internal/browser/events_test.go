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

var everyKind = []pageEventKind{eventLoad, eventNavigated, eventDialog, eventDownload}

func attachScope(t *testing.T, hub *eventHub, name string) {
	t.Helper()
	hub.mu.Lock()
	hub.scopeLocked(name).attached = true
	hub.mu.Unlock()
}

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

	hub.ingest("tab-1", &page.EventFrameNavigated{Frame: &cdp.Frame{ParentID: "parent", URL: "https://ads.test/"}})
	if _, loaded := hub.loadState("tab-1"); !loaded {
		t.Fatal("a subframe navigation reset the tab's load state")
	}
}

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

	if want := fmt.Sprintf("https://example.test/%d", published-1); newest != want {
		t.Fatalf("newest retained event = %q, want %q", newest, want)
	}
}

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

			release()
		})
	}
}

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

const unwatchedEventSettleGrace = 150 * time.Millisecond

func TestAwaitPrearmedSettleAbandonsTheScriptOnNavigation(t *testing.T) {
	tests := []struct {
		name string

		kinds []pageEventKind
		event *pageEvent

		ignoresEvent  bool
		closeTabEarly bool
		awaitFor      time.Duration
		settleCap     time.Duration
	}{
		{
			name:      "the in-page settle resolving first wins",
			kinds:     []pageEventKind{eventNavigated},
			awaitFor:  10 * time.Millisecond,
			settleCap: 5 * time.Second,
		},
		{
			name:      "a navigation abandons an in-page settle that cannot answer",
			kinds:     []pageEventKind{eventNavigated},
			event:     &pageEvent{Kind: eventNavigated},
			awaitFor:  time.Hour,
			settleCap: 5 * time.Second,
		},
		{

			name:         "an unrelated event does not end the settle window",
			kinds:        everyKind,
			event:        &pageEvent{Kind: eventDialog},
			ignoresEvent: true,
			awaitFor:     time.Hour,
			settleCap:    5 * time.Second,
		},
		{
			name:      "a renderer that never replies is bounded by the cap",
			kinds:     []pageEventKind{eventNavigated},
			awaitFor:  time.Hour,
			settleCap: 50 * time.Millisecond,
		},
		{
			name:          "the tab going away ends the window with it",
			kinds:         []pageEventKind{eventNavigated},
			closeTabEarly: true,
			awaitFor:      time.Hour,
			settleCap:     5 * time.Second,
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
			if tt.kinds != nil {
				stream, release := hub.subscribe(tt.kinds, "tab-1")
				defer release()
				sub = stream
			}

			tabCtx, closeTab := context.WithCancel(context.Background())
			defer closeTab()

			started := make(chan struct{})
			awaited := make(chan context.Context, 1)
			liveAtStart := make(chan bool, 1)
			answer := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				awaitPrearmedSettle(tabCtx, sub, tt.settleCap, func(ctx context.Context) {
					awaited <- ctx
					liveAtStart <- ctx.Err() == nil
					close(started)
					select {
					case <-ctx.Done():
					case <-answer:
					case <-time.After(tt.awaitFor):
					}
				})
			}()
			<-started
			if tt.event != nil {
				hub.publish("tab-1", *tt.event)
			}
			if tt.ignoresEvent {
				select {
				case <-done:
					t.Fatal("an event the settle window is not waiting for ended it")
				case <-time.After(unwatchedEventSettleGrace):
				}
				close(answer)
			}
			if tt.closeTabEarly {
				closeTab()
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the settle window never ended")
			}

			if live := <-liveAtStart; !live {
				t.Fatal("the in-page settle was issued on an already-cancelled context")
			}

			awaitCtx := <-awaited
			select {
			case <-awaitCtx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("the abandoned in-page settle outlived the settle window")
			}
		})
	}
}

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
