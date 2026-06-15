package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	cdplaunch "github.com/revitt/agent-browser/internal/cdp"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// newHeadlessTab boots a headless Chrome and navigates to the given HTML data
// URL, returning a tab context plus cleanup. Skips the test when no
// Chrome/Chromium binary is available on the host.
func newHeadlessTab(t *testing.T, html string) (context.Context, func()) {
	t.Helper()
	chromePath, err := cdplaunch.FindChrome("")
	if err != nil {
		t.Skipf("no Chrome/Chromium available: %v", err)
	}

	allocOpts := append([]chromedp.ExecAllocatorOption{},
		chromedp.ExecPath(chromePath),
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("disable-extensions", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)

	// Run navigation on tabCtx itself (chromedp binds the browser to the
	// context it is first Run on); a separate timeout child would cancel that
	// binding when it expires.
	// Percent-encode the markup; unescaped chars such as '#' (CSS colors)
	// otherwise terminate the data: URL at the fragment boundary.
	if err := chromedp.Run(tabCtx,
		chromedp.Navigate("data:text/html,"+url.PathEscape(html)),
		chromedp.WaitReady("body"),
	); err != nil {
		tabCancel()
		allocCancel()
		t.Skipf("headless Chrome navigation failed (environment without browser): %v", err)
	}

	cleanup := func() {
		tabCancel()
		allocCancel()
	}
	return tabCtx, cleanup
}

func evalJSON(t *testing.T, ctx context.Context, expr string, dst any) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			details, _ := json.Marshal(exception)
			return fmt.Errorf("eval exception: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, dst)
	})); err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
}

func resolveRefBox(t *testing.T, ctx context.Context, ref string) snapshot.ElementBox {
	t.Helper()
	box, err := snapshot.ResolveOrRecoverBox(ctx, ref)
	if err != nil {
		t.Fatalf("resolve ref %q: %v", ref, err)
	}
	return box.ElementBox
}

// snapshotRefByQuery takes a snapshot and returns the ref of the first element
// whose name matches query (sets data-agent-browser-ref attributes in-page).
func snapshotRefByQuery(t *testing.T, ctx context.Context, query string) string {
	t.Helper()
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Query: query, Limit: 1})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Elements) == 0 {
		t.Fatalf("no element found for query %q", query)
	}
	return snap.Elements[0].Ref
}

func TestDispatchDragChangesRangeInputValue(t *testing.T) {
	const html = `<input type='range' id='r' min='0' max='100' value='0' style='position:absolute;left:20px;top:20px;width:300px'>`
	ctx, cleanup := newHeadlessTab(t, html)
	defer cleanup()

	// Take a snapshot so the slider gets a stable ref, then resolve its box.
	ref := snapshotRefByQuery(t, ctx, "slider")
	box := resolveRefBox(t, ctx, ref)

	// Drag from the left edge of the slider toward the right edge — value
	// should rise from 0.
	fromX := box.ViewportX - box.Width/2 + 2
	toX := box.ViewportX + box.Width/2 - 2
	y := box.ViewportY
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return dispatchDrag(ctx, fromX, y, toX, y, 15, input.Left)
	})); err != nil {
		t.Fatalf("dispatchDrag: %v", err)
	}

	var value int
	evalJSON(t, ctx, `Number(document.getElementById('r').value)`, &value)
	if value <= 0 {
		t.Fatalf("range value = %d, want > 0 after drag", value)
	}
}

func TestDispatchClickDoubleClickSelectsWord(t *testing.T) {
	const html = `<p id='p' style='position:absolute;left:20px;top:20px;font-size:24px'>selectable</p>`
	ctx, cleanup := newHeadlessTab(t, html)
	defer cleanup()

	var box struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	evalJSON(t, ctx, `(function(){var r=document.getElementById('p').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()`, &box)

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return dispatchClick(ctx, box.X, box.Y, input.Left, 2)
	})); err != nil {
		t.Fatalf("dispatchClick double: %v", err)
	}

	var selected string
	evalJSON(t, ctx, `String(window.getSelection().toString())`, &selected)
	if selected != "selectable" {
		t.Fatalf("selection = %q, want \"selectable\" after double-click", selected)
	}
}

func TestDispatchClickRightFiresContextMenu(t *testing.T) {
	const html = `<div id='d' style='position:absolute;left:10px;top:10px;width:200px;height:80px;background:#eee'></div>` +
		`<script>window.__ctx=0;document.getElementById('d').addEventListener('contextmenu',function(e){e.preventDefault();window.__ctx++;});</script>`
	ctx, cleanup := newHeadlessTab(t, html)
	defer cleanup()

	var box struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	evalJSON(t, ctx, `(function(){var r=document.getElementById('d').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()`, &box)

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return dispatchClick(ctx, box.X, box.Y, input.Right, 1)
	})); err != nil {
		t.Fatalf("dispatchClick right: %v", err)
	}

	var count int
	evalJSON(t, ctx, `Number(window.__ctx||0)`, &count)
	if count < 1 {
		t.Fatalf("contextmenu fired %d times, want >= 1 after right-click", count)
	}
}

func TestMouseDownThenUpCompletesClick(t *testing.T) {
	const html = `<button id='b' style='position:absolute;left:10px;top:10px;width:120px;height:40px'>Go</button>` +
		`<script>window.__clicks=0;document.getElementById('b').addEventListener('click',function(){window.__clicks++;});</script>`
	ctx, cleanup := newHeadlessTab(t, html)
	defer cleanup()

	var box struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	evalJSON(t, ctx, `(function(){var r=document.getElementById('b').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()`, &box)

	// Decomposed press-and-hold: mousePressed then mouseReleased at the same
	// point completes a synthesized click, exactly as MouseDown/MouseUp do.
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MousePressed, box.X, box.Y).
			WithButton(input.Left).WithButtons(1).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		return input.DispatchMouseEvent(input.MouseReleased, box.X, box.Y).
			WithButton(input.Left).WithButtons(0).WithClickCount(1).Do(ctx)
	})); err != nil {
		t.Fatalf("mouse down/up: %v", err)
	}

	var clicks int
	evalJSON(t, ctx, `Number(window.__clicks||0)`, &clicks)
	if clicks < 1 {
		t.Fatalf("click handler fired %d times, want >= 1 after mouse_down + mouse_up", clicks)
	}
}

// TestInPageMouseScriptsActuate exercises the extension-bridge in-page event
// scripts (MouseEventScript / MouseHalfScript / DragScript) the same way the
// bridge does — by evaluating the script with a JSON arg object — confirming
// they actuate standard DOM/pointer events without any CDP Input access.
func TestInPageMouseScriptsActuate(t *testing.T) {
	// A pointer-driven custom slider (role=slider) stands in for the native
	// range input: synthetic in-page pointer events cannot move a native
	// <input type=range> thumb (only real OS events do — which is why the
	// direct-CDP path is primary), but they DO drive custom widgets,
	// drag-and-drop, and canvas/map panning, which is what the bridge path
	// targets. The native-range drag is covered by the CDP path test above.
	const html = `<div id='s' role='slider' aria-label='custom slider' style='position:absolute;left:20px;top:60px;width:300px;height:24px;background:#ccc'></div>` +
		`<button id='b' style='position:absolute;left:20px;top:120px;width:120px;height:40px'>Go</button>` +
		`<div id='d' style='position:absolute;left:20px;top:180px;width:200px;height:60px;background:#eee'></div>` +
		`<script>window.__clicks=0;window.__ctx=0;window.__down=0;window.__up=0;window.__slider=0;window.__sliderDown=false;` +
		`document.getElementById('b').addEventListener('click',function(){window.__clicks++;});` +
		`document.getElementById('b').addEventListener('dblclick',function(){window.__dbl=(window.__dbl||0)+1;});` +
		`document.getElementById('d').addEventListener('contextmenu',function(e){e.preventDefault();window.__ctx++;});` +
		`document.getElementById('d').addEventListener('mousedown',function(){window.__down++;});` +
		`document.getElementById('d').addEventListener('mouseup',function(){window.__up++;});` +
		`var sl=document.getElementById('s');` +
		`sl.addEventListener('pointerdown',function(e){window.__sliderDown=true;});` +
		`sl.addEventListener('pointermove',function(e){if(window.__sliderDown){var r=sl.getBoundingClientRect();window.__slider=Math.round((e.clientX-r.left)/r.width*100);}});` +
		`sl.addEventListener('pointerup',function(e){window.__sliderDown=false;});</script>`
	ctx, cleanup := newHeadlessTab(t, html)
	defer cleanup()

	run := func(script string, arg map[string]any) snapshot.MouseActionResult {
		t.Helper()
		argJSON, _ := json.Marshal(arg)
		var result snapshot.MouseActionResult
		evalJSON(t, ctx, fmt.Sprintf("%s(%s)", script, argJSON), &result)
		if !result.OK {
			t.Fatalf("script result not ok: %q", result.Error)
		}
		return result
	}

	// Left double-click via ref fires a click + dblclick.
	btnRef := snapshotRefByQuery(t, ctx, "Go")
	run(snapshot.MouseEventScript, map[string]any{"ref": btnRef, "button": "left", "click_count": 2})
	var clicks, dbl int
	evalJSON(t, ctx, `Number(window.__clicks||0)`, &clicks)
	evalJSON(t, ctx, `Number(window.__dbl||0)`, &dbl)
	if clicks < 1 || dbl < 1 {
		t.Fatalf("in-page double-click: clicks=%d dbl=%d, want both >= 1", clicks, dbl)
	}

	// Right-click via x,y fires contextmenu.
	var dbox struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	evalJSON(t, ctx, `(function(){var r=document.getElementById('d').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()`, &dbox)
	run(snapshot.MouseEventScript, map[string]any{"x": dbox.X, "y": dbox.Y, "button": "right"})
	var ctxCount int
	evalJSON(t, ctx, `Number(window.__ctx||0)`, &ctxCount)
	if ctxCount < 1 {
		t.Fatalf("in-page right-click contextmenu = %d, want >= 1", ctxCount)
	}

	// Decomposed mouse_down then mouse_up at x,y.
	run(snapshot.MouseHalfScript, map[string]any{"x": dbox.X, "y": dbox.Y, "phase": "down"})
	run(snapshot.MouseHalfScript, map[string]any{"x": dbox.X, "y": dbox.Y, "phase": "up"})
	var down, up int
	evalJSON(t, ctx, `Number(window.__down||0)`, &down)
	evalJSON(t, ctx, `Number(window.__up||0)`, &up)
	if down < 1 || up < 1 {
		t.Fatalf("in-page mouse_down/up: down=%d up=%d, want both >= 1", down, up)
	}

	// In-page drag across the custom pointer-driven slider raises its tracked
	// value from 0 (press at the left edge, move toward the right, release).
	rRef := snapshotRefByQuery(t, ctx, "custom slider")
	rbox := resolveRefBox(t, ctx, rRef)
	run(snapshot.DragScript, map[string]any{
		"from":  map[string]any{"ref": rRef},
		"to":    map[string]any{"x": rbox.ViewportX + rbox.Width/2 - 2, "y": rbox.ViewportY},
		"steps": 15,
	})
	var value int
	evalJSON(t, ctx, `Number(window.__slider||0)`, &value)
	if value <= 0 {
		t.Fatalf("in-page drag custom slider value = %d, want > 0", value)
	}
}
