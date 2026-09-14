package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

// fixtureTTFBDelay is the server think-time the vitals fixture controls. It is
// well above the loopback round trip, so an assertion on it fails if TTFB is
// read from the wrong navigation-timing milestone rather than passing by luck.
const fixtureTTFBDelay = 180 * time.Millisecond

// chromeFaviconPath is requested by the browser, not by the fixture page, so it
// is the one path the no-network assertions below tolerate.
const chromeFaviconPath = "/favicon.ico"

// stableVitalsFixture paints one dominant text block and never moves it, so its
// CLS is zero by construction and its LCP element is unambiguous.
const stableVitalsFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Stable vitals fixture</title></head><body style="margin:0">
<div id="hero" style="height:420px;font-size:64px;background:#dddddd;color:#111111">Hero block</div>
<p id="content" style="font-size:11px">tail</p>
</body></html>`

// shiftingVitalsFixture inserts a 300px banner above the content one frame
// after load. Everything below it moves, which is exactly one layout shift.
const shiftingVitalsFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Shifting vitals fixture</title></head><body style="margin:0">
<div id="hero" style="height:420px;font-size:64px;background:#dddddd;color:#111111">Hero block</div>
<p id="content" style="font-size:11px">tail</p>
<script>
addEventListener('load', function () {
  setTimeout(function () {
    var banner = document.createElement('div');
    banner.id = 'banner';
    banner.style.cssText = 'height:300px;background:#eeeeee';
    document.body.insertBefore(banner, document.body.firstChild);
  }, 60);
});
</script>
</body></html>`

// a11yFixture carries one deliberate failure per rule the audit test asserts
// on: #bbbbbb text on white is roughly 1.9:1 against a 4.5:1 requirement, the
// image has no alt text, and the button has no accessible name.
const a11yFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Accessibility fixture</title></head><body>
<h1>Accessibility fixture</h1>
<p id="low-contrast" style="color:#bbbbbb;background-color:#ffffff;font-size:14px">Contrast here is deliberately too low.</p>
<img id="no-alt" width="40" height="40" src="data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7">
<button id="no-name"></button>
<div id="ok" style="color:#111111;background-color:#ffffff">Readable paragraph.</div>
</body></html>`

// interactiveFixture blocks its click handler long enough that every event in
// the interaction clears the 16 ms event-timing threshold. That is what makes
// the INP grouping observable: pointerdown, pointerup and click are all
// reported, and all three share one interactionId.
const interactiveFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Interactive fixture</title></head><body style="margin:0">
<button id="slow" style="width:240px;height:80px;font-size:24px">Press me</button>
<div id="log"></div>
<script>
document.getElementById('slow').addEventListener('click', function () {
  var until = performance.now() + 220;
  while (performance.now() < until) {}
  var done = document.createElement('p');
  done.id = 'pressed';
  done.textContent = 'handler finished';
  document.getElementById('log').appendChild(done);
});
</script>
</body></html>`

// scrollTargetFixture puts its marker far below the fold, so "did the page move" is a
// question with an answer.
const scrollTargetFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Tall fixture</title></head><body style="margin:0">
<div style="height:4000px;background:#f0f0f0">Filler</div>
<p id="bottom" style="font-size:20px">Bottom marker</p>
</body></html>`

// devtoolsFixtureServer serves the fixtures and records every path it is asked
// for, which is how the audit test proves nothing was fetched for axe.
type devtoolsFixtureServer struct {
	*httptest.Server
	mu    chan struct{}
	paths []string
}

func newDevtoolsFixtureServer(t *testing.T) *devtoolsFixtureServer {
	t.Helper()
	fixture := &devtoolsFixtureServer{mu: make(chan struct{}, 1)}
	fixture.mu <- struct{}{}
	pages := map[string]string{
		"/stable":      stableVitalsFixture,
		"/shift":       shiftingVitalsFixture,
		"/a11y":        a11yFixture,
		"/interactive": interactiveFixture,
		"/tall":        scrollTargetFixture,
	}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.record(r.URL.Path)
		// Chrome asks for a favicon on its own initiative for every page it
		// paints. Answering it keeps the request out of the retry path without
		// pretending the page requested anything.
		if r.URL.Path == chromeFaviconPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		time.Sleep(fixtureTTFBDelay)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

func (f *devtoolsFixtureServer) record(path string) {
	<-f.mu
	f.paths = append(f.paths, path)
	f.mu <- struct{}{}
}

func (f *devtoolsFixtureServer) requested() []string {
	<-f.mu
	out := append([]string(nil), f.paths...)
	f.mu <- struct{}{}
	return out
}

// vitalsUntil reads the vitals repeatedly until the browser has produced the
// metrics the caller is about to assert on.
//
// LCP and FCP exist only once a frame has been presented, and a headless Chrome
// sharing a machine with the rest of this package's live-browser tests can take
// longer than one settle window to get there — the read that ran first then
// reports null and the assertion fails on the machine's load rather than on
// brw. Re-reading costs nothing and hides nothing: every observer is created
// with buffered:true and replays from navigation start, so a later read sees
// exactly what an earlier one would have. A metric brw never reports still
// fails, at the deadline.
func vitalsUntil(t *testing.T, manager *Manager, ctx context.Context, settleMS int, ready func(devtools.Vitals) bool) devtools.Vitals {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		vitals, err := manager.Vitals(ctx, devtools.VitalsOptions{SettleMS: settleMS})
		if err != nil {
			t.Fatalf("vitals: %v", err)
		}
		if ready(vitals) || time.Now().After(deadline) {
			return vitals
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func openFixture(t *testing.T, manager *Manager, url string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if _, err := manager.Open(ctx, url); err != nil {
		t.Fatalf("open %s: %v", url, err)
	}
	return ctx
}

// TestVitalsAgainstRealChrome drives the real Core Web Vitals read against two
// fixtures whose only difference is a layout shift, so a CLS that ignored the
// shift or invented one fails. TTFB is asserted against the server's own
// think-time, and the LCP element against the fixture's dominant block.
func TestVitalsAgainstRealChrome(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)

	tests := []struct {
		name        string
		path        string
		waitFor     string
		wantShifts  bool
		wantLCPPart string
	}{
		{name: "a page that never moves reports no cumulative shift", path: "/stable", wantLCPPart: "hero"},
		{name: "a page with one injected shift reports it", path: "/shift", waitFor: "selector:#banner", wantShifts: true, wantLCPPart: "hero"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := openFixture(t, manager, srv.URL+tt.path)
			if tt.waitFor != "" {
				if err := manager.WaitFor(ctx, tt.waitFor, 15*time.Second); err != nil {
					t.Fatalf("wait for %s: %v", tt.waitFor, err)
				}
			}
			vitals := vitalsUntil(t, manager, ctx, 400, func(v devtools.Vitals) bool {
				return v.LCPMS != nil && v.FCPMS != nil
			})

			if vitals.LCPMS == nil || *vitals.LCPMS <= 0 {
				t.Fatalf("lcp_ms = %v, want a positive largest-contentful-paint time", vitals.LCPMS)
			}
			if !strings.Contains(vitals.LCPElement, tt.wantLCPPart) {
				t.Errorf("lcp_element = %q, want it to name the fixture's %s block", vitals.LCPElement, tt.wantLCPPart)
			}
			if vitals.TTFBMS == nil {
				t.Fatal("ttfb_ms is null; the navigation timing entry was not read")
			}
			// The fixture sleeps before it answers, so anything at or below that
			// sleep means the reading did not come from this navigation.
			floor := float64(fixtureTTFBDelay/time.Millisecond) * 0.8
			if *vitals.TTFBMS < floor {
				t.Errorf("ttfb_ms = %v, want at least %v for a server that waited %v", *vitals.TTFBMS, floor, fixtureTTFBDelay)
			}
			if vitals.FCPMS == nil || *vitals.FCPMS <= 0 {
				t.Errorf("fcp_ms = %v, want a positive first-contentful-paint time", vitals.FCPMS)
			}
			// Headless Chrome observes layout-shift, so a null here is the
			// unavailable path firing on a browser that does support it.
			if vitals.CLS == nil {
				t.Fatalf("cls is null but unavailable = %v; headless Chrome observes layout-shift", vitals.Unavailable)
			}
			switch {
			case tt.wantShifts && *vitals.CLS <= 0:
				t.Errorf("cls = %v with %d shifts, want a non-zero score for the injected banner", *vitals.CLS, vitals.CLSShifts)
			case tt.wantShifts && vitals.CLSShifts == 0:
				t.Errorf("cls_shifts = 0, want the injected banner to be counted")
			case !tt.wantShifts && *vitals.CLS != 0:
				t.Errorf("cls = %v on a page that never moves, want 0", *vitals.CLS)
			}
			if len(vitals.Unavailable) != 0 {
				t.Errorf("unavailable = %v, want nothing on a browser that supports every entry type", vitals.Unavailable)
			}
			if vitals.SettledMS != 400 {
				t.Errorf("settled_ms = %d, want the requested 400", vitals.SettledMS)
			}
			if vitals.Ratings["ttfb"] == "" || vitals.Ratings["cls"] == "" {
				t.Errorf("ratings = %v, want every core metric labelled", vitals.Ratings)
			}
			if !strings.HasSuffix(vitals.URL, tt.path) {
				t.Errorf("url = %q, want the fixture path %q", vitals.URL, tt.path)
			}
		})
	}
}

// TestVitalsClampsTheSettleWindow pins the bound that stops a read being used
// as a sleep.
func TestVitalsClampsTheSettleWindow(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "zero takes the default", in: 0, want: devtools.DefaultVitalsSettleMS},
		{name: "negative takes the default", in: -1, want: devtools.DefaultVitalsSettleMS},
		{name: "an hour is clamped to the ceiling", in: 3600000, want: devtools.MaxVitalsSettleMS},
		{name: "a sane value is kept", in: 700, want: 700},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (devtools.VitalsOptions{SettleMS: tt.in}).Normalize().SettleMS; got != tt.want {
				t.Fatalf("settle_ms = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestAccessibilityAuditAgainstRealChrome runs the embedded engine against a
// fixture whose failures are deliberate, and checks the refs it hands back
// actually point at the offending elements.
func TestAccessibilityAuditAgainstRealChrome(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/a11y")

	result, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{})
	if err != nil {
		t.Fatalf("accessibility audit: %v", err)
	}
	if !strings.HasPrefix(result.Engine, "axe-core ") {
		t.Fatalf("engine = %q, want the axe-core version", result.Engine)
	}
	if result.Note != "" {
		t.Fatalf("note = %q, want the embedded engine to have been the one that ran", result.Note)
	}

	byRule := map[string]devtools.AuditRule{}
	for _, rule := range result.Rules {
		byRule[rule.ID] = rule
	}
	wantElement := map[string]string{
		"color-contrast": "low-contrast",
		"image-alt":      "no-alt",
		"button-name":    "no-name",
	}
	for ruleID, elementID := range wantElement {
		rule, ok := byRule[ruleID]
		if !ok {
			t.Errorf("audit did not report %s; reported %v", ruleID, ruleIDs(result.Rules))
			continue
		}
		if rule.Nodes < 1 || len(rule.Refs) < 1 {
			t.Errorf("%s reported %d nodes and %d refs, want at least one of each", ruleID, rule.Nodes, len(rule.Refs))
			continue
		}
		if rule.Impact == "" {
			t.Errorf("%s carries no impact label", ruleID)
		}
		if rule.Help == "" || rule.HelpURL == "" {
			t.Errorf("%s is missing help text or a help URL: %+v", ruleID, rule)
		}
		// The ref is the whole point of returning refs rather than selectors:
		// it has to resolve, in this page, to the element that failed.
		got, err := manager.Evaluate(ctx, fmt.Sprintf(
			`(function(){var el=document.querySelector('[data-brw-ref=%q]');return el?el.id:'';})()`, rule.Refs[0]))
		if err != nil {
			t.Errorf("resolve %s ref %q: %v", ruleID, rule.Refs[0], err)
			continue
		}
		if got != elementID {
			t.Errorf("%s ref %q resolves to id %q, want %q", ruleID, rule.Refs[0], got, elementID)
		}
	}

	if result.ViolationNodes < len(wantElement) {
		t.Errorf("violation_nodes = %d, want at least the %d deliberate failures", result.ViolationNodes, len(wantElement))
	}
	if result.ByImpact["serious"]+result.ByImpact["critical"] == 0 {
		t.Errorf("by_impact = %v, want the contrast and name failures counted", result.ByImpact)
	}
	if result.Passes == 0 {
		t.Error("passes = 0; the fixture has rules that pass, so the counts are not being read")
	}

	// The bounded summary and the full report are different documents: the
	// report has to carry every node, including the ones max_refs dropped.
	if len(result.Report) == 0 {
		t.Fatal("no full report was produced for the artifact")
	}
	report := string(result.Report)
	for ruleID := range wantElement {
		if !strings.Contains(report, `"`+ruleID+`"`) {
			t.Errorf("full report does not mention %s", ruleID)
		}
	}
	if !strings.Contains(report, `"brw_ref"`) {
		t.Error("full report carries no brw_ref annotations")
	}
}

// TestAccessibilityAuditUsesTheEmbeddedEngineWithoutTheNetwork is the load-
// bearing claim about the vendored bundle: brwd must never pull executable
// script into a page it drives on someone's behalf. The fixture server is the
// page's only reachable origin and records every request; the page's own
// resource timeline records anything Chrome fetched from anywhere.
func TestAccessibilityAuditUsesTheEmbeddedEngineWithoutTheNetwork(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/a11y")

	if _, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{}); err != nil {
		t.Fatalf("accessibility audit: %v", err)
	}

	for _, path := range srv.requested() {
		if path != "/a11y" && path != chromeFaviconPath {
			t.Errorf("fixture server was asked for %q; the audit must fetch nothing", path)
		}
	}
	loaded, err := manager.Evaluate(ctx, `performance.getEntriesByType('resource').map(function(e){return e.name;})`)
	if err != nil {
		t.Fatalf("read the resource timeline: %v", err)
	}
	entries, _ := loaded.([]any)
	for _, entry := range entries {
		name, _ := entry.(string)
		if strings.HasSuffix(name, chromeFaviconPath) {
			continue
		}
		t.Errorf("the page loaded subresource %q; the audit engine must come from the binary", name)
	}

	// And the engine really is in the binary, at the version the package claims.
	version, err := manager.Evaluate(ctx, `String(window.axe && window.axe.version || '')`)
	if err != nil {
		t.Fatalf("read the installed axe version: %v", err)
	}
	if version == "" {
		t.Fatal("no axe engine is present after the audit")
	}
}

// TestAccessibilityAuditBoundsTheSummary: the artifact exists so the MCP answer
// can stay small, which is only true if the caps are actually applied.
func TestAccessibilityAuditBoundsTheSummary(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/a11y")

	full, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if full.Violations < 2 {
		t.Fatalf("fixture produced %d violations, want at least two so a cap of one is observable", full.Violations)
	}

	capped, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{MaxRules: 1, MaxRefs: 1})
	if err != nil {
		t.Fatalf("capped audit: %v", err)
	}
	if len(capped.Rules) != 1 {
		t.Fatalf("max_rules:1 returned %d rules", len(capped.Rules))
	}
	if !capped.Truncated {
		t.Error("truncated is false after dropping rules from the summary")
	}
	if capped.Violations != full.Violations {
		t.Errorf("violations = %d under a cap, want the honest total %d", capped.Violations, full.Violations)
	}
	// Worst first: the one rule that survives a cap of one must not be a minor
	// finding while a critical one was dropped.
	kept := capped.Rules[0].Impact
	for _, rule := range full.Rules {
		if rankOf(rule.Impact) < rankOf(kept) {
			t.Errorf("summary kept a %s rule while %s %q was dropped", kept, rule.Impact, rule.ID)
			break
		}
	}
}

func ruleIDs(rules []devtools.AuditRule) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.ID)
	}
	return out
}

func rankOf(impact string) int {
	switch impact {
	case "critical":
		return 0
	case "serious":
		return 1
	case "moderate":
		return 2
	case "minor":
		return 3
	}
	return 4
}

// TestHighlightAgainstRealChrome covers the one observation here that changes
// the page: it must mark the right box, add exactly one element, and leave
// nothing behind when cleared.
func TestHighlightAgainstRealChrome(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := newDevtoolsFixtureServer(t)
	ctx := openFixture(t, manager, srv.URL+"/a11y")

	audit, err := manager.AccessibilityAudit(ctx, devtools.AuditOptions{Rules: []string{"color-contrast"}})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit.Rules) != 1 || len(audit.Rules[0].Refs) == 0 {
		t.Fatalf("audit produced no contrast ref to highlight: %+v", audit.Rules)
	}
	ref := audit.Rules[0].Refs[0]

	before := overlayCount(t, manager, ctx)
	if before != 0 {
		t.Fatalf("the page already carries %d overlay elements", before)
	}

	marked, err := manager.Highlight(ctx, devtools.HighlightOptions{Ref: ref, Label: "contrast", Color: "orange"})
	if err != nil {
		t.Fatalf("highlight: %v", err)
	}
	if !marked.OK || marked.Active != 1 || len(marked.Marked) != 1 || !marked.Marked[0].Found {
		t.Fatalf("highlight result = %+v, want one found box", marked)
	}
	if marked.Marked[0].Width <= 0 || marked.Marked[0].Height <= 0 {
		t.Errorf("highlighted box has no area: %+v", marked.Marked[0])
	}
	if !strings.Contains(marked.Reversible, devtools.HighlightHostID) {
		t.Errorf("reversible = %q, want it to name the element that was added", marked.Reversible)
	}
	if got := overlayCount(t, manager, ctx); got != 1 {
		t.Fatalf("page carries %d overlay elements after a highlight, want exactly 1", got)
	}
	// The overlay host existing proves nothing: what makes this tool useful is
	// a box drawn over the element, at the element's own geometry.
	drawn := overlayGeometry(t, manager, ctx, "low-contrast")
	if len(drawn.Boxes) != 2 {
		t.Fatalf("overlay drew %d children, want a box and its label: %+v", len(drawn.Boxes), drawn.Boxes)
	}
	if drawn.Boxes[0] != drawn.Target {
		t.Errorf("overlay box = %+v, want it over the element at %+v", drawn.Boxes[0], drawn.Target)
	}
	if drawn.Labels[1] != "contrast" {
		t.Errorf("overlay label = %q, want the caption that was asked for", drawn.Labels[1])
	}
	// The target element itself must be untouched; the overlay is a sibling.
	styled, err := manager.Evaluate(ctx, `document.getElementById('low-contrast').getAttribute('style')`)
	if err != nil {
		t.Fatalf("read the target style: %v", err)
	}
	if styled != "color:#bbbbbb;background-color:#ffffff;font-size:14px" {
		t.Errorf("target inline style = %q, want the fixture's own style unchanged", styled)
	}

	cleared, err := manager.Highlight(ctx, devtools.HighlightOptions{Clear: true})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !cleared.Cleared || cleared.Active != 0 {
		t.Fatalf("clear result = %+v, want the overlay reported gone", cleared)
	}
	if got := overlayCount(t, manager, ctx); got != 0 {
		t.Fatalf("page still carries %d overlay elements after a clear", got)
	}
}

// TestHighlightRejectsWhatItCannotDraw keeps the argument validation honest so
// a caller gets a message rather than a silently empty overlay.
func TestHighlightRejectsWhatItCannotDraw(t *testing.T) {
	tests := []struct {
		name    string
		opts    devtools.HighlightOptions
		wantErr string
	}{
		{name: "no ref and no clear", opts: devtools.HighlightOptions{}, wantErr: "at least one ref"},
		{name: "unknown colour", opts: devtools.HighlightOptions{Ref: "e1", Color: "chartreuse"}, wantErr: "unknown highlight colour"},
		{name: "too many refs", opts: devtools.HighlightOptions{Refs: manyRefs(devtools.MaxHighlightRefs + 1)}, wantErr: "at most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.opts.Normalize()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}

	clearing, err := (devtools.HighlightOptions{Clear: true}).Normalize()
	if err != nil {
		t.Fatalf("clear-only options were rejected: %v", err)
	}
	if clearing.DurationMS != 0 || len(clearing.Refs) != 0 {
		t.Fatalf("clear-only options = %+v, want nothing to draw", clearing)
	}
}

func manyRefs(count int) []string {
	refs := make([]string, 0, count)
	for i := range count {
		refs = append(refs, fmt.Sprintf("e%d", i+1))
	}
	return refs
}

// overlayBox is one rectangle read back out of the page, rounded to whole
// pixels so a sub-pixel layout difference is not mistaken for a misplaced box.
type overlayBox struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type overlayGeometryResult struct {
	Target overlayBox   `json:"target"`
	Boxes  []overlayBox `json:"boxes"`
	Labels []string     `json:"labels"`
}

// overlayGeometry reads what the overlay actually drew, and where, next to the
// element it claims to be marking.
func overlayGeometry(t *testing.T, manager *Manager, ctx context.Context, targetID string) overlayGeometryResult {
	t.Helper()
	value, err := manager.Evaluate(ctx, fmt.Sprintf("(function(){\n  function box(r) { return { left: Math.round(r.left), top: Math.round(r.top), width: Math.round(r.width), height: Math.round(r.height) }; }\n  var host = document.getElementById(%q);\n  var target = document.getElementById(%q);\n  if (!host || !target) return { target: box(new DOMRect()), boxes: [], labels: [] };\n  var boxes = [], labels = [];\n  for (var i = 0; i < host.children.length; i++) {\n    boxes.push(box(host.children[i].getBoundingClientRect()));\n    labels.push(host.children[i].textContent || '');\n  }\n  return { target: box(target.getBoundingClientRect()), boxes: boxes, labels: labels };\n})()", devtools.HighlightHostID, targetID))
	if err != nil {
		t.Fatalf("read the overlay geometry: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("re-encode the overlay geometry: %v", err)
	}
	var result overlayGeometryResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("decode the overlay geometry: %v", err)
	}
	return result
}

func overlayCount(t *testing.T, manager *Manager, ctx context.Context) int {
	t.Helper()
	value, err := manager.Evaluate(ctx, fmt.Sprintf(`document.querySelectorAll('#%s').length`, devtools.HighlightHostID))
	if err != nil {
		t.Fatalf("count overlay elements: %v", err)
	}
	count, ok := value.(float64)
	if !ok {
		t.Fatalf("overlay count = %#v, want a number", value)
	}
	return int(count)
}
