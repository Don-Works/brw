package browser

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/target"
)

func openHTMLInManager(t *testing.T, m *Manager, ctx context.Context, html string) string {
	t.Helper()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html," + url.PathEscape(html)).Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	tabID := string(id)
	m.refs.SetActive(tabID)
	return tabID
}

func TestManagerEvaluateVoidUndefinedReturnsNil(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	openHTMLInManager(t, m, ctx, `<main>evaluate fixture</main>`)

	got, err := m.Evaluate(ctx, `(function(){ document.body.dataset.touched = "yes"; })()`)
	if err != nil {
		t.Fatalf("evaluate undefined side effect: %v", err)
	}
	if got != nil {
		t.Fatalf("undefined evaluate result = %#v, want nil", got)
	}
	touched, err := m.Evaluate(ctx, `document.body.dataset.touched`)
	if err != nil {
		t.Fatalf("evaluate touched: %v", err)
	}
	if touched != "yes" {
		t.Fatalf("side effect marker = %#v, want yes", touched)
	}

	got, err = m.Evaluate(ctx, `void 0`)
	if err != nil {
		t.Fatalf("evaluate void: %v", err)
	}
	if got != nil {
		t.Fatalf("void result = %#v, want nil", got)
	}

	sum, err := m.Evaluate(ctx, `1 + 2`)
	if err != nil {
		t.Fatalf("evaluate scalar: %v", err)
	}
	if sum != float64(3) {
		t.Fatalf("scalar result = %#v, want 3", sum)
	}
}

func TestManagerClickSurfacesDispatchedButNoObservableChange(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	openHTMLInManager(t, m, ctx, `<div id="b" role="button" style="position:absolute;left:20px;top:20px;width:160px;height:60px;border:1px solid black">Noop button</div>
<script>
window.__clicked = 0;
document.getElementById('b').addEventListener('click', function(){ window.__clicked++; });
</script>`)

	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Query: "Noop button", Limit: 1})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Elements) != 1 {
		t.Fatalf("snapshot elements = %d, want 1", len(snap.Elements))
	}

	result, err := m.Click(ctx, snap.Elements[0].Ref)
	if err != nil {
		t.Fatalf("click: %v", err)
	}
	if result.ChangedState == nil || *result.ChangedState {
		t.Fatalf("changed_state = %#v, want false", result.ChangedState)
	}
	if !strings.Contains(result.Warning, "dispatched") || !strings.Contains(result.Warning, "no observable semantic state change") {
		t.Fatalf("warning = %q, want dispatched/no observable warning", result.Warning)
	}
	clicked, err := m.Evaluate(ctx, `Number(window.__clicked || 0)`)
	if err != nil {
		t.Fatalf("evaluate clicked: %v", err)
	}
	if clicked != float64(1) {
		t.Fatalf("click handler count = %#v, want 1", clicked)
	}
}
