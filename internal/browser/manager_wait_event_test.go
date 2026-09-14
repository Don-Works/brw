package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/page"
)

// waitFixture serves a page that can raise a dialog and start a download on
// demand. Nothing site-specific: a blob anchor and window.alert.
func waitFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><title>wait fixture</title><body><p id="marker">fixture</p></body>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWaitForResolvesFromTheEventStream is acceptance for the three conditions
// that used to cost a round trip each time brw checked them. Each case asserts
// the wait was answered by a subscription, not by re-asking: delete the event
// paths and these fall back to the in-page script (or, for a download, to
// nothing at all) and the assertion fails.
func TestWaitForResolvesFromTheEventStream(t *testing.T) {
	m := newHeadlessManager(t)
	fixture := waitFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	// Arm download capture before the download case triggers one.
	if _, err := m.Downloads(tabCtx); err != nil {
		t.Fatalf("arm downloads: %v", err)
	}

	tests := []struct {
		name      string
		condition string
		trigger   string
		timeout   time.Duration
	}{
		{
			// The tab finished loading before this wait was written, so the
			// subscription had already recorded it. The awaited arm — a load that
			// lands while the wait is registered — is covered without a browser by
			// TestReadinessWaitArms.
			name:      "load resolves from the load the subscription already recorded",
			condition: "load",
			timeout:   10 * time.Second,
		},
		{
			name:      "a dialog that opens during the wait resolves from Page.javascriptDialogOpening",
			condition: "dialog:fixture confirm",
			timeout:   10 * time.Second,
			trigger:   `setTimeout(function(){ confirm("fixture confirm text"); }, 150); true`,
		},
		{
			name:      "a dialog that was already answered resolves from the retained ring",
			condition: "dialog:already gone",
			timeout:   10 * time.Second,
			trigger:   `alert("already gone"); true`,
		},
		{
			name:      "download-complete resolves from Browser.downloadProgress",
			condition: "download:evented.txt",
			timeout:   30 * time.Second,
			trigger: `(function(){
				var blob = new Blob(["download-fixture-body"], {type:"text/plain"});
				var a = document.createElement("a");
				a.href = URL.createObjectURL(blob);
				a.download = "evented.txt";
				document.body.appendChild(a);
				a.click();
				return true;
			})()`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.trigger != "" {
				evalCtx, evalCancel := context.WithTimeout(tabCtx, 15*time.Second)
				defer evalCancel()
				if _, err := m.Evaluate(evalCtx, tt.trigger); err != nil {
					t.Fatalf("trigger: %v", err)
				}
			}
			outcome, err := m.WaitForOutcome(tabCtx, tt.condition, tt.timeout)
			if err != nil {
				t.Fatalf("wait for %q: %v", tt.condition, err)
			}
			if !outcome.OK {
				t.Fatalf("outcome = %+v, want ok", outcome)
			}
			if outcome.ResolvedBy != WaitResolvedByEvent {
				t.Fatalf("wait for %q resolved_by = %q, want %q — the wait went back to asking the browser",
					tt.condition, outcome.ResolvedBy, WaitResolvedByEvent)
			}
			// An event-driven wait wakes for the events that were delivered while
			// it was registered, never on a cadence. A handful covers a page that
			// is also loading subresources; a poll would climb with the timeout.
			if outcome.Wakeups > 8 {
				t.Fatalf("wait for %q woke %d times — something in the path is polling", tt.condition, outcome.Wakeups)
			}
		})
	}
}

// TestReadinessWaitArms covers the three ways a readiness wait can end before it
// ever reaches the in-page script, including the one a live fixture cannot pin
// deterministically: the load event arriving while the wait is registered. The
// third return value is the one that matters for the other two — it says whether
// the event path answered at all, and a false there is what sends the caller to
// the document.
func TestReadinessWaitArms(t *testing.T) {
	tests := []struct {
		name        string
		condition   string
		navigated   bool
		loaded      bool
		publishLoad bool
		wantHandled bool
		wantWakeups int
	}{
		{
			name:        "the load event arriving during the wait resolves it",
			condition:   "load",
			navigated:   true,
			publishLoad: true,
			wantHandled: true,
			wantWakeups: 1,
		},
		{
			name:        "a document that already loaded is answered from recorded state",
			condition:   "load",
			navigated:   true,
			loaded:      true,
			wantHandled: true,
		},
		{
			name:      "a tab with no observed lifecycle event goes to the document",
			condition: "load",
		},
		{
			name:      "ready on a navigated-but-unloaded tab goes to the document",
			condition: "ready",
			navigated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			if tt.navigated || tt.loaded {
				attachScope(t, &m.events, "tab-1")
				m.events.markNavigated("tab-1")
			}
			if tt.loaded {
				m.events.markLoaded("tab-1")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			type result struct {
				outcome WaitOutcome
				err     error
				handled bool
			}
			done := make(chan result, 1)
			go func() {
				outcome, err, handled := m.waitForLoadEvent(ctx, ctx, "tab-1", tt.condition, 3*time.Second)
				done <- result{outcome, err, handled}
			}()

			if tt.publishLoad {
				// Long enough that the wait is parked on the stream rather than
				// reading the state it was about to subscribe behind.
				time.Sleep(150 * time.Millisecond)
				m.events.ingest("tab-1", &page.EventLoadEventFired{})
			}

			select {
			case got := <-done:
				if got.handled != tt.wantHandled {
					t.Fatalf("handled = %v, want %v (outcome %+v, err %v)", got.handled, tt.wantHandled, got.outcome, got.err)
				}
				if !tt.wantHandled {
					return
				}
				if got.err != nil {
					t.Fatalf("wait failed: %v", got.err)
				}
				if got.outcome.ResolvedBy != WaitResolvedByEvent {
					t.Fatalf("resolved_by = %q, want %q", got.outcome.ResolvedBy, WaitResolvedByEvent)
				}
				if got.outcome.Wakeups != tt.wantWakeups {
					t.Fatalf("wakeups = %d, want %d", got.outcome.Wakeups, tt.wantWakeups)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the readiness wait never returned")
			}
		})
	}
}

// TestReadinessWaitFallsBackToTheDocumentWhenNoEventWasSeen: brw can attach to a
// tab that loaded long before it arrived, and no load event is coming for that
// document. The wait must read the document rather than block until its timeout.
func TestReadinessWaitFallsBackToTheDocumentWhenNoEventWasSeen(t *testing.T) {
	m := newHeadlessManager(t)
	fixture := waitFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.WaitForOutcome(tabCtx, "load", 10*time.Second); err != nil {
		t.Fatalf("initial load wait: %v", err)
	}

	// Drop everything the hub knows about this tab, modelling an attach that
	// happened after the document had already finished loading.
	m.events.closeScope(opened.Tab.ID)

	outcome, err := m.WaitForOutcome(tabCtx, "load", 10*time.Second)
	if err != nil {
		t.Fatalf("wait for load after losing the event history: %v", err)
	}
	if outcome.ResolvedBy != WaitResolvedByScript {
		t.Fatalf("resolved_by = %q, want %q: with no observed lifecycle event the wait must read the document",
			outcome.ResolvedBy, WaitResolvedByScript)
	}
}

// TestClosingATabLeavesNoLiveSubscription is the leak guard. A subscription that
// outlived its tab would keep retaining that tab's events, and its waiters, for
// the life of the daemon.
func TestClosingATabLeavesNoLiveSubscription(t *testing.T) {
	m := newHeadlessManager(t)
	fixture := waitFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	before := m.events.liveScopes()

	opened, err := m.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.WaitForOutcome(tabCtx, "load", 10*time.Second); err != nil {
		t.Fatalf("wait for load: %v", err)
	}
	if m.events.liveScopes() <= before {
		t.Fatalf("opening a tab did not register a subscription (scopes %d -> %d)", before, m.events.liveScopes())
	}

	// Cancel the tab's chromedp context directly rather than going through
	// CloseTab. A tab can die without brw asking — the user closes it, a popup
	// self-closes, the renderer crashes — and the subscription has to go with the
	// context itself, not only with brw's own bookkeeping.
	m.mu.RLock()
	tab := m.tabContexts[opened.Tab.ID]
	m.mu.RUnlock()
	if tab.cancel == nil {
		t.Fatal("tab context was never published")
	}
	tab.cancel()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.events.liveScopes() <= before && m.events.liveSubscribers() == 0 {
			if err := m.CloseTab(ctx, opened.Tab.ID); err != nil {
				t.Logf("close tab after cancelling its context: %v", err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("after the tab context ended: scopes=%d (want <= %d) subscribers=%d (want 0)",
		m.events.liveScopes(), before, m.events.liveSubscribers())
}

// TestEventWaitLatencyBeatsThePollingFallback is the measurement recorded in
// docs/benchmarks.md. Both paths answer the same question — "has this download
// finished?" — against the same registry; only how they notice differs. The
// polling comparison is the extension transport's real cadence, not a strawman:
// it is what a transport with no subscription to attach has to do.
func TestEventWaitLatencyBeatsThePollingFallback(t *testing.T) {
	const (
		samples      = 9
		pollInterval = 50 * time.Millisecond
	)

	measure := func(t *testing.T, wait func(m *Manager, ctx context.Context) error) time.Duration {
		t.Helper()
		latencies := make([]time.Duration, 0, samples)
		for i := range samples {
			m := &Manager{}
			guid := fmt.Sprintf("g%d", i)
			seedDownload(m, guid, "bench.bin", string(downloadStateInProgress), time.Now())

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			done := make(chan time.Time, 1)
			go func() {
				_ = wait(m, ctx)
				done <- time.Now()
			}()
			// Let the wait register before the completion lands, so what is
			// measured is the delay between the state changing and the wait
			// returning — not the time to start waiting.
			time.Sleep(120 * time.Millisecond)
			completed := time.Now()
			m.handleDownloadEventForTab("", downloadCompletedEvent(guid))
			latencies = append(latencies, (<-done).Sub(completed))
			cancel()
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		return latencies[samples/2]
	}

	evented := measure(t, func(m *Manager, ctx context.Context) error {
		_, err := m.waitForDownload(ctx, "", 5*time.Second)
		return err
	})
	polled := measure(t, func(m *Manager, ctx context.Context) error {
		return pollForCompletedDownload(ctx, m, pollInterval, 5*time.Second)
	})

	t.Logf("download wait median latency: event-driven=%s polled@%s=%s (%.1fx faster)",
		evented, pollInterval, polled, float64(polled)/float64(evented))

	// A 50 ms poll averages half its interval before it notices; the event path
	// should be an order of magnitude inside that. Assert a conservative margin
	// so a loaded CI host cannot fail a genuine win.
	if evented*4 >= polled {
		t.Fatalf("event-driven median=%s vs polled median=%s; want at least 4x faster", evented, polled)
	}
}

// TestLoadWaitLatencyBeatsTheInPageProbe measures the other half of the change,
// also recorded in docs/benchmarks.md. Both answer "is this document loaded?"
// about the same tab: the event path reads state the subscription already
// maintains, the script path pays a CDP round trip to ask the document.
func TestLoadWaitLatencyBeatsTheInPageProbe(t *testing.T) {
	m := newHeadlessManager(t)
	fixture := waitFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabID := opened.Tab.ID
	tabCtx := WithTabID(ctx, tabID)
	if _, err := m.WaitForOutcome(tabCtx, "load", 10*time.Second); err != nil {
		t.Fatalf("initial load wait: %v", err)
	}
	_, pageCtx, release, err := m.activeContextWithTimeout(tabCtx, 20*time.Second)
	if err != nil {
		t.Fatalf("tab context: %v", err)
	}
	defer release()

	const samples = 9
	measure := func(t *testing.T, once func() error) time.Duration {
		t.Helper()
		latencies := make([]time.Duration, 0, samples)
		for range samples {
			started := time.Now()
			if err := once(); err != nil {
				t.Fatalf("wait: %v", err)
			}
			latencies = append(latencies, time.Since(started))
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		return latencies[samples/2]
	}

	evented := measure(t, func() error {
		outcome, err := m.WaitForOutcome(tabCtx, "load", 5*time.Second)
		if err == nil && outcome.ResolvedBy != WaitResolvedByEvent {
			t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, WaitResolvedByEvent)
		}
		return err
	})
	scripted := measure(t, func() error {
		_, err := snapshot.WaitForCondition(pageCtx, "ready", 5000)
		return err
	})

	t.Logf("load wait median latency: event-driven=%s in-page probe=%s (%.1fx faster)",
		evented, scripted, float64(scripted)/float64(evented))
	if evented*2 >= scripted {
		t.Fatalf("event-driven median=%s vs in-page median=%s; want at least 2x faster", evented, scripted)
	}
}

// pollForCompletedDownload is the pre-subscription wait, kept only as the
// benchmark's control: it re-reads the registry on a fixed cadence and so cannot
// notice a completion any sooner than its next tick.
func pollForCompletedDownload(ctx context.Context, m *Manager, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		m.downloadsMu.Lock()
		for _, entry := range m.downloads {
			if entry.State == string(downloadStateCompleted) {
				m.downloadsMu.Unlock()
				return nil
			}
		}
		m.downloadsMu.Unlock()
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
