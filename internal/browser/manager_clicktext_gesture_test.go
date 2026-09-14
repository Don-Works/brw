package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// A handler registered with addEventListener is unreadable from page script:
// there is no API that hands back the listeners on a node, so brw cannot look at
// a button and predict that clicking it will call window.open. click_text used to
// resolve that button, dispatch a synthetic MouseEvent, and return ok:true —
// while the gesture-gated call inside the handler was dropped, because a
// dispatched event grants no transient user activation. The click landed, the
// handler ran, and the thing the handler was for did not happen.
//
// Chrome is launched here WITHOUT chromedp's default --disable-popup-blocking:
// with that flag on, window.open succeeds with no activation at all and a
// fixture meant to prove activation proves nothing. Each fixture records
// navigator.userActivation.isActive as its handler sees it, so the assertion
// names the property rather than a downstream symptom, and then checks the
// symptom too.
func TestClickTextActuatesGestureGatedListenersItCannotSee(t *testing.T) {
	tests := []struct {
		name string
		body string
		text string
	}{
		{
			// The listener is on the button itself, registered with
			// addEventListener: no onclick attribute and no onclick property for
			// __abRequiresTrustedClick to read.
			name: "addEventListener on the button",
			body: `<button id="b">Open child</button>
			       <script>document.getElementById('b').addEventListener('click', gated);</script>`,
			text: "Open child",
		},
		{
			// Event delegation: the listener is on document, so even the button's
			// own node carries nothing to inspect. Activation has to survive the
			// bubble to the delegated handler.
			name: "delegated listener on document",
			body: `<button id="b" data-open="1">Open child</button>
			       <script>document.addEventListener('click', function(e){ if (e.target.dataset.open) gated(); });</script>`,
			text: "Open child",
		},
		{
			// A non-button clickable, resolved through the role/tabindex pass.
			name: "addEventListener on a role=button div",
			body: `<div id="b" role="button" tabindex="0">Open child</div>
			       <script>document.getElementById('b').addEventListener('click', gated);</script>`,
			text: "Open child",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := newGestureFixtureSite(t, tt.body)
			m := newHeadlessManagerWith(t, chromedp.Flag("disable-popup-blocking", false))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			opened, err := m.Open(ctx, site.URL)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			tabCtx := WithTabID(ctx, opened.Tab.ID)
			if _, err := m.ClickText(tabCtx, snapshot.ClickTextOptions{Text: tt.text}); err != nil {
				t.Fatalf("click text: %v", err)
			}

			handlerRan, active := readGestureProbe(t, m, tabCtx)
			if !handlerRan {
				t.Fatal("the click never reached the addEventListener handler")
			}
			if !active {
				t.Fatal("the handler ran without transient user activation, so every gesture-gated call it makes is dropped " +
					"while click_text still reports success")
			}
			waitForChildTab(t, m, ctx, site.URL+"/child")
		})
	}
}

// An inline onclick attribute is the shape brw CAN see: it is deferred and
// actuated through real CDP input. Kept alongside the invisible shapes so the
// two paths are asserted to agree about activation rather than only the one the
// fix touched.
func TestClickTextActuatesAGestureGatedInlineHandler(t *testing.T) {
	// The attribute has to spell window.open for __abRequiresTrustedClick's
	// pattern to fire; calling a helper that spells it would take the ordinary
	// in-page path and test nothing about deferral.
	site := newGestureFixtureSite(t, `<button onclick="window.__gestureProbe.ran=true;`+
		`window.__gestureProbe.active=Boolean(navigator.userActivation&amp;&amp;navigator.userActivation.isActive);`+
		`window.open('/child','_blank')">Open child</button>`)
	m := newHeadlessManagerWith(t, chromedp.Flag("disable-popup-blocking", false))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.ClickText(tabCtx, snapshot.ClickTextOptions{Text: "Open child"}); err != nil {
		t.Fatalf("click text: %v", err)
	}
	handlerRan, active := readGestureProbe(t, m, tabCtx)
	if !handlerRan || !active {
		t.Fatalf("inline gesture-gated handler: ran=%v activation=%v, want both true", handlerRan, active)
	}
	waitForChildTab(t, m, ctx, site.URL+"/child")
}

// brw_click on a ref takes the same single-evaluate in-page fast path as
// click_text whenever the resolved box is not flagged as needing a trusted
// gesture — and that flag is read from the same unreadable-listener sniffing. A
// ref click on a gesture-gated addEventListener handler therefore had the
// identical defect.
func TestClickRefActuatesGestureGatedListenersItCannotSee(t *testing.T) {
	site := newGestureFixtureSite(t, `<button id="b">Open child</button>
		<script>document.getElementById('b').addEventListener('click', gated);</script>`)
	m := newHeadlessManagerWith(t, chromedp.Flag("disable-popup-blocking", false))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, el := range snap.Elements {
		if el.Role == "button" && el.Name == "Open child" {
			ref = el.Ref
			break
		}
	}
	if ref == "" {
		t.Fatalf("no button ref for the fixture: %+v", snap.Elements)
	}
	if _, err := m.Click(tabCtx, ref); err != nil {
		t.Fatalf("click %s: %v", ref, err)
	}
	handlerRan, active := readGestureProbe(t, m, tabCtx)
	if !handlerRan {
		t.Fatal("the click never reached the addEventListener handler")
	}
	if !active {
		t.Fatal("the handler ran without transient user activation, so every gesture-gated call it makes is dropped " +
			"while brw_click still reports success")
	}
	waitForChildTab(t, m, ctx, site.URL+"/child")
}

// newGestureFixtureSite serves a page whose gated() helper records whether the
// browser considered a user gesture to be in progress when the handler ran, and
// then makes a genuinely activation-gated call.
func newGestureFixtureSite(t *testing.T, body string) *httptest.Server {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/child" {
			fmt.Fprint(w, `<!doctype html><title>Child window</title><main>child</main>`)
			return
		}
		fmt.Fprintf(w, `<!doctype html><title>Opener</title><body>
			<script>
			window.__gestureProbe = { ran: false, active: false };
			function gated() {
			  window.__gestureProbe.ran = true;
			  window.__gestureProbe.active = Boolean(navigator.userActivation && navigator.userActivation.isActive);
			  window.open('/child', '_blank');
			}
			</script>
			%s</body>`, body)
	}))
	t.Cleanup(site.Close)
	return site
}

func readGestureProbe(t *testing.T, m *Manager, tabCtx context.Context) (ran bool, active bool) {
	t.Helper()
	value, err := m.Evaluate(tabCtx, `JSON.stringify(window.__gestureProbe)`)
	if err != nil {
		t.Fatalf("read gesture probe: %v", err)
	}
	raw, _ := value.(string)
	var probe struct {
		Ran    bool `json:"ran"`
		Active bool `json:"active"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("decode gesture probe %q: %v", raw, err)
	}
	return probe.Ran, probe.Active
}

func waitForChildTab(t *testing.T, m *Manager, ctx context.Context, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tabs, err := m.ListTabs(ctx)
		if err != nil {
			t.Fatalf("list tabs: %v", err)
		}
		for _, tab := range tabs {
			if tab.URL == want {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no tab at %s: the popup blocker dropped window.open because the click carried no user activation", want)
}
