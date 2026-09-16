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
)

// serveParityFixture serves HTML and returns its URL. Each test names its own
// page so the assertion is made against what the page saw.
func serveParityFixture(t *testing.T, page string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The override is only real if the page sees it: Intl formatting and Date have
// to resolve in the chosen locale and zone, not just for CDP to have accepted
// the command.
//
// This asserts Intl/Date, NOT navigator.language, and that is deliberate.
// Emulation.setLocaleOverride and Emulation.setUserAgentOverride's
// accept_language are two different overrides with two different effects, and a
// Linux Chromium 152 probe shows it plainly:
//
//	setLocaleOverride(en_GB)      -> Intl locale en-GB, Date "Greenwich Mean
//	                                 Time"; navigator.language STAYS en-US
//	accept_language(fr-FR)        -> navigator.language fr-FR; Intl locale
//	                                 STAYS en-US
//
// brw exposes both on purpose (brw_set_locale and brw_set_user_agent's
// accept_language), so the locale tool is tested for what it actually drives.
// Asserting navigator.language here passed on macOS Chrome 153, which happens to
// apply the locale override to it too, and failed on Linux CI: that is a
// browser difference the test should not paper over.
func TestSetLocaleIsWhatThePageReports(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t, `<html><body><h1>locale</h1></body></html>`))

	if _, err := m.SetLocale(ctx, LocaleOptions{Locale: "en-GB", Timezone: "Europe/London"}); err != nil {
		t.Fatalf("SetLocale: %v", err)
	}
	if got := evaluateString(t, m, ctx, `Intl.DateTimeFormat().resolvedOptions().locale`); got != "en-GB" {
		t.Fatalf("Intl locale = %q, want en-GB", got)
	}
	if got := evaluateString(t, m, ctx, `Intl.DateTimeFormat().resolvedOptions().timeZone`); got != "Europe/London" {
		t.Fatalf("page time zone = %q, want Europe/London", got)
	}
	// Date formatting follows the zone, which is the observable a caller reads.
	if got := evaluateString(t, m, ctx, `new Date(2020,0,1).toString()`); !strings.Contains(got, "Greenwich Mean Time") && !strings.Contains(got, "GMT") {
		t.Fatalf("Date.toString() = %q, want it to resolve in London", got)
	}

	// A timezone-only request must leave the locale alone, because the two are
	// independent Emulation commands.
	if _, err := m.SetLocale(ctx, LocaleOptions{Timezone: "America/New_York"}); err != nil {
		t.Fatalf("SetLocale timezone only: %v", err)
	}
	if got := evaluateString(t, m, ctx, `Intl.DateTimeFormat().resolvedOptions().locale`); got != "en-GB" {
		t.Fatalf("after a timezone-only override Intl locale = %q, want it left at en-GB", got)
	}
	if got := evaluateString(t, m, ctx, `Intl.DateTimeFormat().resolvedOptions().timeZone`); got != "America/New_York" {
		t.Fatalf("page time zone = %q, want America/New_York", got)
	}

	if _, err := m.SetLocale(ctx, LocaleOptions{Clear: true}); err != nil {
		t.Fatalf("clear locale: %v", err)
	}
}

// A registered init script has to run before the NEXT document's own scripts,
// which is the whole difference between this and brw_evaluate.
func TestInitScriptRunsBeforeTheNextDocument(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url := serveParityFixture(t, `<html><body><h1>init</h1></body></html>`)
	emulationTab(t, m, ctx, url)

	if got := evaluateString(t, m, ctx, `typeof window.__brwInit === "undefined" ? "absent" : "present"`); got != "absent" {
		t.Fatalf("before adding a script, window.__brwInit was %q, want absent", got)
	}

	added, err := m.InitScript(ctx, InitScriptOptions{Action: "add", Source: `window.__brwInit = 42;`})
	if err != nil {
		t.Fatalf("add init script: %v", err)
	}
	if added.Added == nil || added.Added.ID == "" {
		t.Fatalf("add returned no identifier: %+v", added)
	}

	// Reload the document: the script must run before its own content.
	if _, err := m.NavigateTo(ctx, url); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := evaluateString(t, m, ctx, `window.__brwInit === 42 ? "yes" : "no"`); got != "yes" {
		t.Fatalf("window.__brwInit was not 42 after reload (check said %q)", got)
	}

	listed, err := m.InitScript(ctx, InitScriptOptions{Action: "list"})
	if err != nil {
		t.Fatalf("list init scripts: %v", err)
	}
	if len(listed.Scripts) != 1 {
		t.Fatalf("listed %d scripts, want 1", len(listed.Scripts))
	}

	if _, err := m.InitScript(ctx, InitScriptOptions{Action: "remove", ID: added.Added.ID}); err != nil {
		t.Fatalf("remove init script: %v", err)
	}
	if _, err := m.NavigateTo(ctx, url); err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if got := evaluateString(t, m, ctx, `typeof window.__brwInit === "undefined" ? "absent" : "present"`); got != "absent" {
		t.Fatalf("window.__brwInit was %q after removal, want absent", got)
	}
}

const touchFixture = `<html><body style="margin:0">
<div id="pad" style="width:320px;height:320px;background:#eee"></div>
<script>
window.touchLog = [];
document.addEventListener('touchstart', function (e) { window.touchLog.push('start:' + Math.round(e.touches[0].clientX)); }, {passive: true});
document.addEventListener('touchmove', function (e) { window.touchLog.push('move:' + Math.round(e.touches[0].clientX)); }, {passive: true});
document.addEventListener('touchend', function () { window.touchLog.push('end'); });
</script></body></html>`

// A tap and a swipe have to reach the page as real touch events, not as a mouse
// click and not as nothing.
func TestTouchDispatchReachesThePage(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t, touchFixture))

	x, y := 40.0, 40.0
	if _, err := m.Touch(ctx, TouchOptions{Action: "tap", X: &x, Y: &y}); err != nil {
		t.Fatalf("tap: %v", err)
	}
	log := evaluateString(t, m, ctx, `(window.touchLog || []).join(",")`)
	if log == "" {
		t.Fatal("the page received no touch events from a tap")
	}
	t.Logf("tap log: %s", log)

	if _, err := m.Evaluate(ctx, `window.touchLog = []`); err != nil {
		t.Fatalf("reset log: %v", err)
	}
	// The swipe is VERTICAL on purpose. A horizontal drag across the viewport is
	// Chrome's overscroll swipe-to-navigate gesture on a touch-enabled profile,
	// and on the Linux CI Chrome it navigated the tab away mid-test, so the next
	// read found the fixture's window global gone. A vertical swipe exercises the
	// same touchmove path without triggering that gesture.
	toX, toY := 40.0, 260.0
	if _, err := m.Touch(ctx, TouchOptions{Action: "swipe", X: &x, Y: &y, ToX: &toX, ToY: &toY, DurationMS: 120}); err != nil {
		t.Fatalf("swipe: %v", err)
	}
	log = evaluateString(t, m, ctx, `(window.touchLog || []).join(",")`)
	if log == "" {
		t.Fatal("the page received no touch events from a swipe")
	}
	if got := evaluateString(t, m, ctx, `(window.touchLog || []).filter(function (e) { return e.indexOf("move:") === 0; }).length`); got == "0" {
		t.Fatalf("a swipe produced no touchmove events: %s", log)
	}
	t.Logf("swipe log: %s", log)
}

// A trace has to come back as parseable trace JSON with at least one event, and
// a CPU profile as parseable profile JSON: "Chrome accepted the command" is not
// the same as "a capture was produced".
func TestProfileCapturesTraceAndCPU(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t, `<html><body><h1>profile</h1><script>for (let i=0;i<20000;i++){ Math.sqrt(i); }</script></body></html>`))

	if _, err := m.Profile(ctx, ProfileOptions{Action: "start", Kind: ProfileKindTrace}); err != nil {
		t.Fatalf("start trace: %v", err)
	}
	if _, err := m.Evaluate(ctx, `(() => { let s = 0; for (let i = 0; i < 50000; i++) { s += Math.sqrt(i); } return s; })()`); err != nil {
		t.Fatalf("drive work during trace: %v", err)
	}
	trace, err := m.Profile(ctx, ProfileOptions{Action: "stop", Kind: ProfileKindTrace})
	if err != nil {
		t.Fatalf("stop trace: %v", err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(trace.Data, &events); err != nil {
		t.Fatalf("trace is not a JSON array: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("a trace was captured with zero events")
	}

	if _, err := m.Profile(ctx, ProfileOptions{Action: "start", Kind: ProfileKindCPU}); err != nil {
		t.Fatalf("start cpu profile: %v", err)
	}
	if _, err := m.Evaluate(ctx, `(() => { let s = 0; for (let i = 0; i < 200000; i++) { s += Math.sqrt(i); } return s; })()`); err != nil {
		t.Fatalf("drive work during profile: %v", err)
	}
	cpu, err := m.Profile(ctx, ProfileOptions{Action: "stop", Kind: ProfileKindCPU})
	if err != nil {
		t.Fatalf("stop cpu profile: %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(cpu.Data, &profile); err != nil {
		t.Fatalf("cpu profile is not JSON: %v", err)
	}
	if _, ok := profile["nodes"]; !ok {
		t.Fatalf("cpu profile has no nodes: %v", profile)
	}
}

// The fiber walker is checked against a synthetic fiber graph rather than a real
// React bundle: the walker reads the same __reactFiber$/__reactContainer$ keys
// React writes, and this fixture writes them directly so the test needs no
// network and no framework.
const reactFixture = `<html><body>
<div id="root"><button id="go" data-brw-ref="e1">Go</button></div>
<script>
  var go = document.getElementById('go');
  function App() {}
  App.displayName = 'App';
  function Button() {}
  Button.displayName = 'Button';
  var hostFiber = { type: 'button', key: null, return: null, child: null, sibling: null, memoizedProps: { id: 'go' }, memoizedState: null };
  var compFiber = { type: Button, key: 'b1', return: null, child: hostFiber, sibling: null,
    memoizedProps: { disabled: false, label: 'hi', onClick: function () {} },
    memoizedState: { memoizedState: 1, next: { memoizedState: 'x', next: null } } };
  hostFiber.return = compFiber;
  var appFiber = { type: App, key: null, return: null, child: compFiber, sibling: null, memoizedProps: {}, memoizedState: null };
  compFiber.return = appFiber;
  var rootFiber = { type: null, key: null, return: null, child: appFiber, sibling: null, memoizedProps: null, memoizedState: null };
  appFiber.return = rootFiber;
  go['__reactFiber$brwtest'] = hostFiber;
  document.getElementById('root')['__reactContainer$brwtest'] = rootFiber;
</script>
</body></html>`

func TestReactTreeAndInspectReadTheFiberTree(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t, reactFixture))

	tree, err := m.React(ctx, ReactOptions{Action: "tree"})
	if err != nil {
		t.Fatalf("react tree: %v", err)
	}
	if !tree.Present {
		t.Fatalf("no React root detected: %+v", tree)
	}
	names := map[string]bool{}
	for _, n := range tree.Nodes {
		names[n.Name] = true
	}
	for _, want := range []string{"HostRoot", "App", "Button", "button"} {
		if !names[want] {
			t.Fatalf("tree is missing %q: %+v", want, tree.Nodes)
		}
	}

	inspected, err := m.React(ctx, ReactOptions{Action: "inspect", Target: "e1"})
	if err != nil {
		t.Fatalf("react inspect: %v", err)
	}
	if inspected.Inspect == nil {
		t.Fatalf("inspect returned no component: %+v", inspected)
	}
	if inspected.Inspect.Component != "Button" {
		t.Fatalf("inspect component = %q, want Button", inspected.Inspect.Component)
	}
	if got := inspected.Inspect.Props["label"]; got != "hi" {
		t.Fatalf("inspect prop label = %q, want hi", got)
	}
	if len(inspected.Inspect.HookKinds) != 2 {
		t.Fatalf("inspect hooks = %v, want two", inspected.Inspect.HookKinds)
	}
}

func TestReactReportsAbsenceWithoutError(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t, `<html><body><h1>plain</h1></body></html>`))
	result, err := m.React(ctx, ReactOptions{Action: "tree"})
	if err != nil {
		t.Fatalf("react tree on a plain page: %v", err)
	}
	if result.Present || result.Count != 0 {
		t.Fatalf("expected no React root, got %+v", result)
	}
}

func TestScrollToBringsAnElementIntoView(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveParityFixture(t,
		`<html><body style="margin:0"><div style="height:4000px"></div><button id="far" data-brw-ref="e9">Far</button></body></html>`))

	if got := evaluateBool(t, m, ctx, `document.getElementById('far').getBoundingClientRect().top > window.innerHeight`); !got {
		t.Fatal("the element should start below the viewport")
	}
	if _, err := m.ScrollTo(ctx, "e9"); err != nil {
		t.Fatalf("ScrollTo by ref: %v", err)
	}
	if got := evaluateBool(t, m, ctx, `(function(){ var r = document.getElementById('far').getBoundingClientRect(); return r.top >= 0 && r.bottom <= window.innerHeight + 1; })()`); !got {
		t.Fatal("ScrollTo did not bring the element into the viewport")
	}
	if _, err := m.ScrollTo(ctx, "#far"); err != nil {
		t.Fatalf("ScrollTo by selector: %v", err)
	}
}

// Check has to land the state the caller asked for, both ways, without a click.
func TestCheckSetsAnExplicitState(t *testing.T) {
	m := newHeadlessManager(t)
	ctx := context.Background()
	if _, err := m.Open(ctx, serveParityFixture(t, `<input id="c" type="checkbox" aria-label="agree">`)); err != nil {
		t.Fatal(err)
	}
	want := true
	if _, err := m.Check(ctx, CheckOptions{Query: "agree", Checked: &want}); err != nil {
		t.Fatalf("check true: %v", err)
	}
	if got := evaluateString(t, m, ctx, `document.getElementById('c').checked ? "yes" : "no"`); got != "yes" {
		t.Fatalf("checkbox checked = %q, want yes", got)
	}
	off := false
	if _, err := m.Check(ctx, CheckOptions{Query: "agree", Checked: &off}); err != nil {
		t.Fatalf("check false: %v", err)
	}
	if got := evaluateString(t, m, ctx, `document.getElementById('c').checked ? "yes" : "no"`); got != "no" {
		t.Fatalf("checkbox checked = %q, want no", got)
	}
}
