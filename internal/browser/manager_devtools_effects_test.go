package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/snapshot"
)

// The tests in this file cover what the three developer tools promise to do TO
// the page — leave no observers behind, intercept no clicks, move nothing
// uninvited, record what they did — as opposed to what they report about it.

// TestVitalsCountsInteractionsNotEvents: one press is one interaction. The
// event-timing timeline reports pointerdown, pointerup and click separately and
// the Core Web Vitals definition groups them by interactionId, so an ungrouped
// count over-reports by roughly three and slides the percentile index off the
// slowest event onto a faster sibling.
func TestVitalsCountsInteractionsNotEvents(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/interactive")

	// A trusted CDP input dispatch, not the synthesised MouseEvent the ordinary
	// click path builds in page script: event timing only assigns an
	// interactionId to real user input, so a synthetic click produces no
	// interaction to group.
	_, tabCtx, cancel, err := manager.devtoolsContext(ctx, 60*time.Second)
	if err != nil {
		t.Fatalf("resolve the tab: %v", err)
	}
	defer cancel()
	if err := chromedp.Run(tabCtx, chromedp.MouseClickXY(120, 40)); err != nil {
		t.Fatalf("press the fixture button: %v", err)
	}
	// The handler appends its marker last, so this is the signal the blocking
	// work ran rather than the press having missed the button.
	if err := manager.WaitFor(ctx, "selector:#pressed", 15*time.Second); err != nil {
		t.Fatalf("the fixture handler never ran: %v", err)
	}

	vitals := vitalsUntil(t, manager, ctx, 600, func(v devtools.Vitals) bool { return v.Interactions > 0 })
	if vitals.Interactions == 0 {
		t.Skip("this browser retained no event-timing entries for the press; there is nothing to group")
	}
	if vitals.Interactions != 1 {
		t.Errorf("interactions = %d after exactly one press, want 1: the event-timing entries are not grouped by interactionId", vitals.Interactions)
	}
	if vitals.INPMS == nil {
		t.Fatal("inp_ms is null although interactions were counted")
	}
	// The handler blocks for 220ms, so the interaction's worst event cannot be
	// quicker than that by much. An ungrouped read can report a faster sibling.
	if *vitals.INPMS < 150 {
		t.Errorf("inp_ms = %v for a handler that blocked 220ms, want at least 150", *vitals.INPMS)
	}
	if vitals.Ratings["inp"] == "" || vitals.Ratings["inp"] == "unknown" {
		t.Errorf("ratings[inp] = %q, want a label once an interaction was measured", vitals.Ratings["inp"])
	}
}

// TestVitalsDisconnectsItsObservers is the "leaves nothing in the page" half of
// what brw_vitals advertises. The script's observers are unreachable from
// outside it, so the page counts constructions and disconnections through a
// wrapper installed before the read.
func TestVitalsDisconnectsItsObservers(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/stable")

	// The wrapper keeps the real observer and the real static
	// supportedEntryTypes, or the script would see a browser that supports
	// nothing and never construct one at all.
	if _, err := manager.Evaluate(ctx, `(function(){
  var Real = window.PerformanceObserver;
  window.__brwObs = { made: 0, off: 0 };
  function Counting(cb) {
    var real = new Real(cb);
    window.__brwObs.made++;
    this.observe = function(o) { return real.observe(o); };
    this.disconnect = function() { window.__brwObs.off++; return real.disconnect(); };
    this.takeRecords = function() { return real.takeRecords(); };
  }
  Counting.supportedEntryTypes = Real.supportedEntryTypes;
  window.PerformanceObserver = Counting;
  return true;
})()`); err != nil {
		t.Fatalf("install the observer counter: %v", err)
	}

	if _, err := manager.Vitals(ctx, devtools.VitalsOptions{SettleMS: 400}); err != nil {
		t.Fatalf("vitals: %v", err)
	}

	counts := observerCounts(t, manager, ctx)
	if counts.Made == 0 {
		t.Fatal("the read constructed no PerformanceObserver at all, so this proves nothing")
	}
	if counts.Off != counts.Made {
		t.Fatalf("the read constructed %d observers and disconnected %d; a pure read leaves none attached to the page", counts.Made, counts.Off)
	}
}

type observerCountResult struct {
	Made int `json:"made"`
	Off  int `json:"off"`
}

func observerCounts(t *testing.T, manager *Manager, ctx context.Context) observerCountResult {
	t.Helper()
	value, err := manager.Evaluate(ctx, `window.__brwObs`)
	if err != nil {
		t.Fatalf("read the observer counts: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("re-encode the observer counts: %v", err)
	}
	var counts observerCountResult
	if err := json.Unmarshal(encoded, &counts); err != nil {
		t.Fatalf("decode the observer counts: %v", err)
	}
	return counts
}

// TestVitalsReportsWhatItCannotObserve: PerformanceObserver.observe() with one
// unsupported entry type warns and returns rather than throwing, so a browser
// without layout-shift has to be detected from supportedEntryTypes. Without
// that the metric reads 0 and is rated "good" on a browser that never looked.
func TestVitalsReportsWhatItCannotObserve(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/shift")
	if err := manager.WaitFor(ctx, "selector:#banner", 15*time.Second); err != nil {
		t.Fatalf("wait for the injected banner: %v", err)
	}

	// Hide one entry type from the page, the way a browser without it would.
	if _, err := manager.Evaluate(ctx, `(function(){
  var kept = Array.prototype.filter.call(PerformanceObserver.supportedEntryTypes || [], function(t) {
    return t !== 'layout-shift';
  });
  Object.defineProperty(PerformanceObserver, 'supportedEntryTypes', {
    configurable: true, get: function() { return kept; }
  });
  return kept.indexOf('layout-shift') < 0;
})()`); err != nil {
		t.Fatalf("hide the layout-shift entry type: %v", err)
	}

	vitals := vitalsUntil(t, manager, ctx, 400, func(v devtools.Vitals) bool { return v.FCPMS != nil })
	if !containsString(vitals.Unavailable, "layout-shift") {
		t.Fatalf("unavailable = %v, want it to name the entry type this browser does not support", vitals.Unavailable)
	}
	// This fixture really does shift, which is what makes a 0 the exact lie:
	// a perfect score, rated good, from a browser that never observed one.
	if vitals.CLS != nil {
		t.Errorf("cls = %v with layout-shift unobservable, want null", *vitals.CLS)
	}
	if vitals.Ratings["cls"] != "unknown" {
		t.Errorf("ratings[cls] = %q, want \"unknown\" rather than a verdict on an unmeasured metric", vitals.Ratings["cls"])
	}
	// Hiding one entry type must not cost the metrics that are still there.
	if vitals.FCPMS == nil {
		t.Error("fcp_ms is null; hiding one entry type lost the others")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestHighlightDoesNotSwallowClicks pins the promise that makes the overlay safe
// to leave up. The boxes are drawn over the element, so without
// pointer-events:none a later click lands on the overlay instead of the page.
func TestHighlightDoesNotSwallowClicks(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/interactive")

	ref := refForText(t, manager, ctx, "Press me")
	if _, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref, Label: "here"}); err != nil {
		t.Fatalf("highlight: %v", err)
	}

	// What the human's next click would hit, at the centre of the marked box.
	hit, err := manager.Evaluate(ctx, `(function(){
  var el = document.getElementById('slow');
  var r = el.getBoundingClientRect();
  var at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
  return at ? String(at.id || at.tagName) : '';
})()`)
	if err != nil {
		t.Fatalf("hit-test the highlighted element: %v", err)
	}
	if hit != "slow" {
		t.Fatalf("a click at the centre of the highlighted element would hit %q, want the element itself: the overlay is intercepting input", hit)
	}

	// And it still works as a click, not only as a hit test.
	if _, err := manager.Click(ctx, ref); err != nil {
		t.Fatalf("click through the overlay: %v", err)
	}
	if err := manager.WaitFor(ctx, "selector:#pressed", 15*time.Second); err != nil {
		t.Fatalf("the page's own handler never ran under the overlay: %v", err)
	}
	if got := overlayCount(t, manager, ctx); got != 1 {
		t.Errorf("overlay elements after the click = %d, want the highlight still up", got)
	}
}

// TestHighlightScrollsOnlyWhenAsked covers both halves of the default: a
// read-shaped call must not move someone's page, and scroll:true must move it.
func TestHighlightScrollsOnlyWhenAsked(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/tall")

	ref := refForText(t, manager, ctx, "Bottom marker")

	quiet, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref})
	if err != nil {
		t.Fatalf("highlight: %v", err)
	}
	if offset := scrollY(t, manager, ctx); offset != 0 {
		t.Fatalf("the page scrolled to %v without scroll:true", offset)
	}
	if len(quiet.Marked) != 1 || !quiet.Marked[0].Found {
		t.Fatalf("highlight result = %+v, want the bottom marker found", quiet)
	}
	if quiet.Marked[0].InViewport {
		t.Fatal("the fixture's bottom marker is already on screen, so scrolling to it would prove nothing")
	}

	moved, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref, Scroll: true})
	if err != nil {
		t.Fatalf("highlight with scroll: %v", err)
	}
	if offset := scrollY(t, manager, ctx); offset <= 0 {
		t.Fatalf("scroll:true left the page at %v", offset)
	}
	if !moved.Marked[0].InViewport {
		t.Fatalf("marked = %+v, want the element reported on screen after scrolling to it", moved.Marked[0])
	}
	// The coordinates are re-read after the scroll, so they describe where the
	// element is now rather than where it was when the call started.
	geometry := overlayGeometry(t, manager, ctx, "bottom")
	if len(geometry.Boxes) == 0 {
		t.Fatal("no overlay box after a scrolling highlight")
	}
	if geometry.Boxes[0] != geometry.Target {
		t.Errorf("overlay box = %+v, want it over the element at %+v", geometry.Boxes[0], geometry.Target)
	}
}

// TestHighlightExpiresOnItsOwnSchedule drives the auto-clear timer against a
// real page. The tool description, the CLI's "clears itself in Nms" line and
// expires_in_ms all promise an overlay that removes itself, and every other
// highlight test asserts on the drawing rather than on time passing, so a
// highlight that reported a duration and then never expired would leave a box
// on a human's screen with all of them green.
//
// The rows go both ways on purpose: a timer that never fires fails the first,
// and a timer that fires when nothing asked for one, or fires immediately,
// fails the other two.
func TestHighlightExpiresOnItsOwnSchedule(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/interactive")
	ref := refForText(t, manager, ctx, "Press me")

	// Long enough that the round trip cannot be mistaken for the timer, short
	// enough to wait out; and a duration no test run outlives, for the rows
	// that need a timer pending while the overlay stays up.
	const expiringMS = 750
	const pendingMS = 120000
	// How long the "it stayed up" rows watch before believing it.
	const persistFor = 3 * time.Second

	tests := []struct {
		name       string
		durationMS int
		wantExpiry bool
	}{
		{name: "a duration removes the overlay with no second call", durationMS: expiringMS, wantExpiry: true},
		{name: "no duration leaves the overlay up", durationMS: 0},
		{name: "a pending duration leaves the overlay up until it is due", durationMS: pendingMS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := time.Now()
			drawn, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref, DurationMS: tt.durationMS})
			if err != nil {
				t.Fatalf("highlight: %v", err)
			}
			// Neither the box nor a timer still counting down may reach the next row.
			t.Cleanup(func() {
				if _, err := manager.Highlight(ctx, devtools.HighlightOptions{Clear: true}); err != nil {
					t.Errorf("clear: %v", err)
				}
			})
			if drawn.Active != 1 {
				t.Fatalf("highlight result = %+v, want one box drawn", drawn)
			}
			if drawn.ExpiresMS != tt.durationMS {
				t.Errorf("expires_in_ms = %d, want %d", drawn.ExpiresMS, tt.durationMS)
			}

			window := persistFor
			if tt.wantExpiry {
				// Generous: this is a wall-clock wait on a machine sharing its
				// cores with the rest of this package's live-browser tests, and
				// what is under test is that the timer fires at all.
				window = 60 * time.Second
			}
			gone, waited := waitForOverlayGone(t, manager, ctx, window)
			if gone != tt.wantExpiry {
				if tt.wantExpiry {
					t.Fatalf("the overlay was still up %s after a %dms duration, so it never cleared itself", window, tt.durationMS)
				}
				t.Fatalf("the overlay vanished after %s with duration_ms %d, and nothing asked it to", waited, tt.durationMS)
			}
			if !tt.wantExpiry {
				return
			}
			// setTimeout does not fire early and the timer is armed inside the
			// highlight call, so a removal quicker than the duration measured
			// from before that call was not this timer.
			if wanted := time.Duration(tt.durationMS) * time.Millisecond; time.Since(started) < wanted {
				t.Errorf("the overlay went after %s, sooner than the %s that was asked for", time.Since(started), wanted)
			}
			// The listeners that keep the boxes aligned go with the element, or
			// the page keeps paying for an overlay it no longer has.
			if !highlightStateGone(t, manager, ctx) {
				t.Error("window.__brwHighlight survived the expiry, so the scroll and resize listeners are still attached")
			}
			// An expiry is the page's own doing, so a clear afterwards has
			// nothing left to remove and must not report a removal.
			cleared, err := manager.Highlight(ctx, devtools.HighlightOptions{Clear: true})
			if err != nil {
				t.Fatalf("clear after the expiry: %v", err)
			}
			if cleared.Cleared {
				t.Error("clear reported removing an overlay the duration had already taken")
			}
		})
	}
}

// waitForOverlayGone watches the page for up to within, and reports whether the
// overlay disappeared and how long that took.
func waitForOverlayGone(t *testing.T, manager *Manager, ctx context.Context, within time.Duration) (bool, time.Duration) {
	t.Helper()
	started := time.Now()
	for {
		if overlayCount(t, manager, ctx) == 0 {
			return true, time.Since(started)
		}
		if time.Since(started) >= within {
			return false, time.Since(started)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// highlightStateGone reports whether the script's window state was cleared. It
// is reduced to a boolean in the page because the state holds the listener
// functions, which have no value to bring back.
func highlightStateGone(t *testing.T, manager *Manager, ctx context.Context) bool {
	t.Helper()
	value, err := manager.Evaluate(ctx, `!window.__brwHighlight`)
	if err != nil {
		t.Fatalf("read the highlight state: %v", err)
	}
	cleared, ok := value.(bool)
	if !ok {
		t.Fatalf("highlight state = %#v, want a boolean", value)
	}
	return cleared
}

func refForText(t *testing.T, manager *Manager, ctx context.Context, text string) string {
	t.Helper()
	found, err := manager.Find(ctx, snapshot.FindOptions{Text: text, TextContent: true, IncludeHidden: true})
	if err != nil {
		t.Fatalf("find %q: %v", text, err)
	}
	if len(found.Elements) == 0 {
		t.Fatalf("find %q returned nothing", text)
	}
	return found.Elements[0].Ref
}

func scrollY(t *testing.T, manager *Manager, ctx context.Context) float64 {
	t.Helper()
	value, err := manager.Evaluate(ctx, `window.scrollY`)
	if err != nil {
		t.Fatalf("read the scroll offset: %v", err)
	}
	offset, ok := value.(float64)
	if !ok {
		t.Fatalf("scrollY = %#v, want a number", value)
	}
	return offset
}

// TestDevtoolsObservationsReachTheTrace: a human watching brwd's activity has to
// see the one new tool that changes the page. Evaluate, Click, Open and Read all
// record; these three recorded nothing at all.
func TestDevtoolsObservationsReachTheTrace(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/a11y")

	audit, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{Rules: []string{"color-contrast"}})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit.Rules) == 0 || len(audit.Rules[0].Refs) == 0 {
		t.Fatalf("audit produced no ref to highlight: %+v", audit.Rules)
	}
	ref := audit.Rules[0].Refs[0]

	manager.ClearTrace()
	if _, err := manager.Vitals(ctx, devtools.VitalsOptions{SettleMS: 250}); err != nil {
		t.Fatalf("vitals: %v", err)
	}
	if _, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{Rules: []string{"color-contrast"}}); err != nil {
		t.Fatalf("second audit: %v", err)
	}
	if _, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref, Label: "look here"}); err != nil {
		t.Fatalf("highlight: %v", err)
	}

	entries := manager.GetTrace().Entries
	byAction := map[string]TraceEntry{}
	for _, entry := range entries {
		byAction[entry.Action] = entry
	}

	tests := []struct {
		name          string
		action        string
		wantObserved  bool
		wantRef       string
		wantURLSuffix string
	}{
		{name: "vitals is an observation", action: TraceActionVitals, wantObserved: true, wantURLSuffix: "/a11y"},
		{name: "the audit is an observation", action: TraceActionAudit, wantObserved: true, wantURLSuffix: "/a11y"},
		{name: "highlight is an action on the page", action: TraceActionHighlight, wantRef: ref},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := byAction[tt.action]
			if !ok {
				t.Fatalf("no %q entry in the trace; it recorded %v", tt.action, devtoolsTraceActions(entries))
			}
			if !entry.OK {
				t.Errorf("%q recorded as failed: %q", tt.action, entry.Error)
			}
			if entry.TabID == "" {
				t.Errorf("%q recorded with no tab id, so it would be unscoped in a shared daemon", tt.action)
			}
			if IsObservationAction(tt.action) != tt.wantObserved {
				t.Errorf("IsObservationAction(%q) = %v, want %v", tt.action, IsObservationAction(tt.action), tt.wantObserved)
			}
			if tt.wantURLSuffix != "" && !strings.HasSuffix(entry.Text, tt.wantURLSuffix) {
				t.Errorf("%q recorded text %q, want the page it observed", tt.action, entry.Text)
			}
			if tt.wantRef != "" && entry.Ref != tt.wantRef {
				t.Errorf("%q recorded ref %q, want %q", tt.action, entry.Ref, tt.wantRef)
			}
		})
	}

	// The caption is caller text, and the trace is served over the HTTP control
	// plane to every caller of a shared daemon.
	if strings.Contains(byAction[TraceActionHighlight].Text, "look here") {
		t.Errorf("the highlight caption reached the trace: %q", byAction[TraceActionHighlight].Text)
	}
	// And a replay has to explain the entry rather than list it back as an
	// unknown action a human should have recognised.
	if reason := skipReason(TraceActionHighlight); strings.HasPrefix(reason, "not a replayable action") {
		t.Errorf("replay describes a highlight as %q", reason)
	}
}

func devtoolsTraceActions(entries []TraceEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Action)
	}
	return out
}

// TestEvaluateAwaitRefusesAnEmptyCompletionValue: every expression these tools
// send resolves to an object, so an undefined value means the script never ran.
// Decoding it into a zero struct would answer a vitals read with cls 0 and every
// pointer null, which reads exactly like a clean page.
func TestEvaluateAwaitRefusesAnEmptyCompletionValue(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/stable")
	_, tabCtx, cancel, err := manager.devtoolsContext(ctx, 10*time.Second)
	if err != nil {
		t.Fatalf("resolve the tab: %v", err)
	}
	defer cancel()

	tests := []struct {
		name       string
		expression string
		wantErr    bool
	}{
		{name: "undefined", expression: `(function(){ return undefined; })()`, wantErr: true},
		{name: "an explicit null", expression: `(function(){ return null; })()`, wantErr: true},
		{name: "a promise resolving to nothing", expression: `Promise.resolve(undefined)`, wantErr: true},
		{name: "a real object decodes", expression: `(function(){ return { url: "https://example.test/" }; })()`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var vitals devtools.Vitals
			err := evaluateAwait(tabCtx, tt.expression, &vitals)
			if tt.wantErr {
				if !errors.Is(err, devtools.ErrNoObservation) {
					t.Fatalf("error = %v, want the named empty-observation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if vitals.URL != "https://example.test/" {
				t.Fatalf("url = %q, want the value the expression produced", vitals.URL)
			}
		})
	}
}
