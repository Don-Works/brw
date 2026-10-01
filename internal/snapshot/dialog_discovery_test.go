package snapshot_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

func TestFrontierDiscoversStyledControlsInUnfocusedDialog(t *testing.T) {
	page := `<!doctype html><body><nav>` + strings.Repeat(`<button aria-haspopup="menu">Background action</button>`, 100) + `</nav>
<section role="dialog" style="position:fixed;left:100px;top:100px;width:400px;padding:20px;background:white">
<label style="display:block;position:relative;padding:32px">Allow external members
<input id="external" type="checkbox" style="position:absolute;left:0;top:0;width:24px;height:24px;opacity:0">
</label>
<div role="checkbox" aria-label="Notify members" aria-checked="false" tabindex="0">Notify members</div>
<input type="checkbox" aria-label="Removed control" style="visibility:hidden">
<div style="opacity:0"><button>Invisible descendant</button></div>
<button id="add">Add</button></section><output>Nothing added</output>
<div style="opacity:0;position:fixed;left:700px;top:100px"><section role="dialog"><button>Ghost Add</button></section></div>
<script>document.querySelector('nav button').focus();
document.querySelector('#add').onclick=()=>{if(document.querySelector('#external').checked){document.querySelector('section').remove();document.querySelector('output').textContent='1 member added';}};</script>`
	srv := serveHTML(t, page)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, cancelCtx := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelCtx()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	frontier, err := snapshot.EvaluateWithOptions(ctx, snapshot.NormalizeOptions(snapshot.SnapshotOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	var checkbox, add *snapshot.Element
	for i := range frontier.Elements {
		el := &frontier.Elements[i]
		if el.Role == "checkbox" && el.Name == "Allow external members" {
			checkbox = el
		}
		if el.Role == "button" && el.Name == "Add" {
			add = el
		}
		if el.Name == "Notify members" && (el.Checked == nil || *el.Checked) {
			t.Fatalf("ARIA checked state missing: %+v", el)
		}
		if (el.Name == "Removed control" || el.Name == "Invisible descendant") && slices.Contains(el.Signals, "task-scope") {
			t.Fatal("removed control promoted into active task")
		}
	}
	if checkbox == nil || add == nil || !slices.Contains(checkbox.Signals, "pointer-actionable") || !slices.Contains(add.Signals, "task-scope") {
		t.Fatalf("dialog controls missing: %+v", frontier.Elements)
	}
	if checkbox.Visible || !strings.Contains(snapshot.RenderCompact(frontier), "pointer-actionable") {
		t.Fatal("transparent native control misrepresented")
	}
	if !slices.Contains(frontier.Elements[0].Signals, "task-scope") {
		t.Fatal("background control outranked dialog")
	}
	observed, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	observedAdd := findByName(observed.Elements, "Add")
	if observedAdd == nil || !slices.Contains(observedAdd.Signals, "task-scope") {
		t.Fatal("action observation lost dialog scope")
	}
	for _, ref := range []string{checkbox.Ref, add.Ref} {
		box, err := snapshot.ResolveBox(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(ctx, chromedp.MouseClickXY(box.ViewportX, box.ViewportY)); err != nil {
			t.Fatal(err)
		}
	}
	var result string
	if err := chromedp.Run(ctx, chromedp.Text("output", &result, chromedp.ByQuery)); err != nil || result != "1 member added" {
		t.Fatalf("workflow did not complete: result=%q err=%v", result, err)
	}
}
