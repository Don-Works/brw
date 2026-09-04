package snapshot_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

func TestFrontierKeepsOffViewportActionsInActiveModal(t *testing.T) {
	const page = `<!doctype html><html><head><title>modal frontier</title></head>
<body style="margin:0;min-height:2000px">
  <button>Unrelated page action</button>
  <aside id="drawer" role="dialog" aria-modal="true" aria-label="Task drawer" tabindex="-1"
    style="position:fixed;right:0;top:0;width:360px;height:500px;overflow:auto">
    <div style="height:1400px;display:grid;align-content:space-between">
      <h2>Task drawer</h2><button id="confirm">Confirm task</button>
    </div>
  </aside>
  <script>document.getElementById('drawer').focus()</script>
</body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	frontier, err := snapshot.EvaluateWithOptions(ctx, snapshot.NormalizeOptions(snapshot.SnapshotOptions{}))
	if err != nil {
		t.Fatalf("frontier snapshot: %v", err)
	}
	confirm := findByName(frontier.Elements, "Confirm task")
	if confirm == nil {
		t.Fatalf("off-viewport action in active modal was omitted: %v", names(frontier.Elements))
	}
	if confirm.InViewport {
		t.Fatal("fixture precondition broken: confirm button unexpectedly in viewport")
	}
	foundScopeSignal := false
	for _, signal := range confirm.Signals {
		if signal == "task-scope" {
			foundScopeSignal = true
			break
		}
	}
	if !foundScopeSignal {
		t.Fatalf("off-viewport modal action missing task-scope signal: %+v", confirm)
	}

	strict, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all", ViewportOnly: true})
	if err != nil {
		t.Fatalf("strict viewport snapshot: %v", err)
	}
	if findByName(strict.Elements, "Confirm task") != nil {
		t.Fatal("explicit viewport-only all-mode snapshot should remain strict outside frontier mode")
	}
}

func TestClickXYTargetsPaintedLeafInsideOpenShadowRoot(t *testing.T) {
	const page = `<!doctype html><html><head><title>shadow click</title></head><body>
<div id="host"></div><p id="status">waiting</p>
<script>
  const root = document.getElementById('host').attachShadow({mode:'open'});
  root.innerHTML = '<button id="save" style="width:180px;height:48px">Save shadow value</button>';
  root.getElementById('save').addEventListener('click', () => document.getElementById('status').textContent = 'saved');
</script></body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	button := findByName(snap.Elements, "Save shadow value")
	if button == nil {
		t.Fatalf("shadow button missing from snapshot: %v", names(snap.Elements))
	}
	box, err := snapshot.ResolveBox(ctx, button.Ref)
	if err != nil {
		t.Fatalf("resolve shadow button: %v", err)
	}
	clicked, err := snapshot.ClickXY(ctx, box.ViewportX, box.ViewportY)
	if err != nil || !clicked.OK {
		t.Fatalf("click shadow button = %+v, %v", clicked, err)
	}
	var status string
	if err := chromedp.Run(ctx, chromedp.Text("#status", &status, chromedp.ByID)); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "saved" {
		t.Fatalf("shadow handler did not run; status=%q", status)
	}
}

func TestResolveBoxMarksSecurityGatedClicks(t *testing.T) {
	const page = `<!doctype html><html><head><title>trusted clicks</title></head><body>
<button id="popup" aria-haspopup="dialog">Open popup</button>
<button id="ordinary">Ordinary action</button>
<a id="blank" href="about:blank" target="_blank">Open tab</a>
</body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "Open popup", want: true},
		{name: "Open tab", want: true},
		{name: "Ordinary action", want: false},
	} {
		el := findByName(snap.Elements, tc.name)
		if el == nil {
			t.Fatalf("element %q missing", tc.name)
		}
		box, err := snapshot.ResolveBox(ctx, el.Ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", tc.name, err)
		}
		if box.RequiresTrusted != tc.want {
			t.Fatalf("%q requires_trusted=%v, want %v", tc.name, box.RequiresTrusted, tc.want)
		}
	}
}

func TestClickTextFallsBackToVisibleListenerBackedTextLeaf(t *testing.T) {
	const page = `<!doctype html><html><head><title>text leaf click</title></head><body>
<div id="modal" style="position:fixed;inset:40px;width:320px;height:180px;background:white">
  <div id="footer"><p>Close</p></div>
</div><p id="status">open</p>
<script>document.getElementById('footer').addEventListener('click',()=>{document.getElementById('modal').remove();document.getElementById('status').textContent='closed'})</script>
</body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	exact := true
	clicked, err := snapshot.ClickText(ctx, snapshot.ClickTextOptions{Text: "Close", Exact: exact})
	if err != nil || !clicked.OK {
		t.Fatalf("click listener-backed text leaf = %+v, %v", clicked, err)
	}
	var status string
	if err := chromedp.Run(ctx, chromedp.Text("#status", &status, chromedp.ByID)); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "closed" {
		t.Fatalf("delegated close listener did not run; status=%q", status)
	}
}

func TestScrollDispatchesWindowScrollEventForStickyPageLogic(t *testing.T) {
	const page = `<!doctype html><html><head><title>scroll event</title></head>
<body style="margin:0;height:2400px"><nav id="menu" style="position:absolute;top:0;height:40px">Menu</nav>
<script>
  window.__scrollEvents = 0;
  window.addEventListener('scroll', () => {
    window.__scrollEvents++;
    document.getElementById('menu').style.top = window.scrollY + 'px';
  });
</script></body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	result, err := snapshot.Scroll(ctx, "down")
	if err != nil || !result.OK || !result.Changed {
		t.Fatalf("scroll = %+v, %v", result, err)
	}
	var state struct {
		Events int     `json:"events"`
		Top    float64 `json:"top"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({events:window.__scrollEvents,top:document.getElementById('menu').getBoundingClientRect().top})`, &state)); err != nil {
		t.Fatalf("read scroll state: %v", err)
	}
	if state.Events < 1 {
		t.Fatal("scroll moved the viewport but did not notify page scroll listeners")
	}
	if state.Top < -1 || state.Top > 1 {
		t.Fatalf("sticky-page listener did not keep menu in view; top=%v", state.Top)
	}
}

func TestPressKeyFallbackPreservesEventsAndNumberInputDefault(t *testing.T) {
	const page = `<!doctype html><html><head><title>background key</title></head><body>
<input id="number" type="number" value="42">
<script>
  window.__keys = [];
  const input = document.getElementById('number');
  for (const type of ['keydown','keyup','input','change']) {
    input.addEventListener(type, event => window.__keys.push({type:event.type,key:event.key,keyCode:event.keyCode,value:input.value}));
  }
  input.focus();
</script></body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	var result struct {
		OK      bool `json:"ok"`
		Changed bool `json:"changed"`
	}
	expression := fmt.Sprintf(`%s({key:'ArrowUp',code:'ArrowUp',keyCode:38,modifiers:0})`, snapshot.PressKeyFallbackScript)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &result)); err != nil {
		t.Fatalf("fallback press: %v", err)
	}
	if !result.OK || !result.Changed {
		t.Fatalf("fallback result = %+v", result)
	}
	var state struct {
		Value  string `json:"value"`
		Events []struct {
			Type    string `json:"type"`
			Key     string `json:"key"`
			KeyCode int    `json:"keyCode"`
		} `json:"events"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({value:document.getElementById('number').value,events:window.__keys})`, &state)); err != nil {
		t.Fatalf("read key state: %v", err)
	}
	if state.Value != "43" {
		t.Fatalf("number value = %q, want 43", state.Value)
	}
	if len(state.Events) < 4 || state.Events[0].Type != "keydown" || state.Events[0].Key != "ArrowUp" || state.Events[0].KeyCode != 38 || state.Events[len(state.Events)-1].Type != "keyup" {
		t.Fatalf("keyboard/input event sequence = %+v", state.Events)
	}
}

func TestFillDispatchesBeforeInputBeforeContentEditableMutation(t *testing.T) {
	const page = `<!doctype html><html><head><title>rich editor fill</title></head><body>
<main id="editor" contenteditable="true">old value</main>
<script>
  const editor = document.getElementById('editor');
  window.__fillEvents = [];
  editor.addEventListener('beforeinput', () => window.__fillEvents.push('before:' + editor.textContent));
  editor.addEventListener('input', () => window.__fillEvents.push('input:' + editor.textContent));
</script></body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	editor := findByName(snap.Elements, "old value")
	if editor == nil {
		t.Fatalf("editor missing from snapshot: %v", names(snap.Elements))
	}
	if err := snapshot.Fill(ctx, editor.Ref, "new value", true); err != nil {
		t.Fatalf("fill editor: %v", err)
	}
	var events []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fillEvents`, &events)); err != nil {
		t.Fatalf("read fill events: %v", err)
	}
	if len(events) < 2 || events[0] != "before:old value" || events[1] != "input:new value" {
		t.Fatalf("fill event order = %v", events)
	}
}

func TestAssertValueTreatsStructuralEmptyContentEditableAsEmpty(t *testing.T) {
	const page = `<!doctype html><html><head><title>empty rich editor</title></head><body>
<div id="editor" role="textbox" contenteditable="true" aria-label="Composer"><p><br></p></div>
</body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	editor := findByName(snap.Elements, "Composer")
	if editor == nil {
		t.Fatalf("composer missing from snapshot: %v", names(snap.Elements))
	}
	if err := snapshot.EvalAssert(ctx, snapshot.AssertValueScript, editor.Ref, "", 100); err != nil {
		t.Fatalf("structural empty editor did not assert empty: %v", err)
	}
	if err := snapshot.Fill(ctx, editor.Ref, "prepared draft", true); err != nil {
		t.Fatalf("fill editor: %v", err)
	}
	if err := snapshot.EvalAssert(ctx, snapshot.AssertValueScript, editor.Ref, "prepared draft", 100); err != nil {
		t.Fatalf("filled editor value assertion failed: %v", err)
	}
}

func TestHoverScriptTriggersDelegatedMouseenterMenus(t *testing.T) {
	const page = `<!doctype html><html><head><title>delegated hover</title></head><body>
<ul id="menu" role="menu" aria-label="Main menu"><li id="enabled" role="menuitem" aria-haspopup="menu" tabindex="0">Enabled<ul id="submenu" role="menu" aria-label="Downloads menu" hidden><li id="downloads" role="menuitem" aria-haspopup="menu" tabindex="0">Downloads<ul id="formats" role="menu" aria-label="Formats menu" hidden><li role="menuitem">PDF</li></ul></li></ul></li></ul>
<script>
  window.__hoverTransitions = [];
  document.getElementById('menu').addEventListener('mouseover', event => {
    const item = event.target.closest('[role="menuitem"]');
    window.__hoverTransitions.push({ target: item && item.id, related: event.relatedTarget && event.relatedTarget.id });
    if (item && item.id === 'enabled' && !event.relatedTarget?.closest?.('#enabled')) {
      document.getElementById('submenu').hidden = false;
    }
    if (item && item.id === 'downloads' && event.relatedTarget?.closest?.('#enabled')) {
      document.getElementById('formats').hidden = false;
    }
  });
</script></body></html>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	enabled := findByName(snap.Elements, "Enabled")
	if enabled == nil {
		t.Fatalf("menu item missing from snapshot: %v", names(snap.Elements))
	}
	var result struct {
		OK           bool `json:"ok"`
		DelayedHover bool `json:"delayed_hover"`
	}
	expression := fmt.Sprintf(`%s(%q)`, snapshot.HoverElementScript, enabled.Ref)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &result)); err != nil || !result.OK {
		t.Fatalf("hover script = %+v, %v", result, err)
	}
	if !result.DelayedHover {
		t.Fatal("ARIA menu item was not marked for delayed hover settling")
	}
	box, err := snapshot.ResolveBox(ctx, enabled.Ref)
	if err != nil {
		t.Fatalf("resolve menu item: %v", err)
	}
	if !box.DelayedHover {
		t.Fatal("resolved ARIA menu item was not marked for delayed hover settling")
	}
	var hidden bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('submenu').hidden`, &hidden)); err != nil {
		t.Fatal(err)
	}
	if hidden {
		t.Fatal("delegated mouseover/mouseenter menu did not open")
	}
	updated, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeHidden: true})
	if err != nil {
		t.Fatalf("updated snapshot: %v", err)
	}
	downloads := findByName(updated.Elements, "Downloads")
	if downloads == nil {
		t.Fatalf("nested menu item missing from snapshot: %v", names(updated.Elements))
	}
	expression = fmt.Sprintf(`%s(%q)`, snapshot.HoverElementScript, downloads.Ref)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &result)); err != nil || !result.OK {
		t.Fatalf("nested hover script = %+v, %v", result, err)
	}
	var nested struct {
		Hidden     bool `json:"hidden"`
		RelatedWas bool `json:"related_was_enabled"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({ hidden: document.getElementById('formats').hidden, related_was_enabled: window.__hoverTransitions.some(x => x.target === 'downloads' && x.related === 'enabled') })`, &nested)); err != nil {
		t.Fatal(err)
	}
	if nested.Hidden || !nested.RelatedWas {
		t.Fatalf("nested hover transition = %+v, want visible submenu with enabled as related target", nested)
	}
}
