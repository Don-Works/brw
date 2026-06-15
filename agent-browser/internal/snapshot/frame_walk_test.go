package snapshot_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/cdp"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// parentWithSameOriginIframe serves a page that embeds a same-origin srcdoc
// iframe holding a button. The iframe is offset from the top-left corner so the
// test can prove that resolving the in-frame button's ref produces coordinates
// translated into top-level viewport space (not the frame-local space).
const parentWithSameOriginIframe = `<!doctype html>
<html><head><meta charset="utf-8"><title>frame host</title></head>
<body style="margin:0;padding:0">
  <button id="top-btn">Top Button</button>
  <iframe id="embed" style="position:absolute;left:120px;top:90px;width:300px;height:200px;border:0"
    srcdoc="<!doctype html><html><body style='margin:0'><button id='inner' style='position:absolute;left:10px;top:20px;width:140px;height:40px'>Inner Frame Button</button></body></html>">
  </iframe>
</body></html>`

// newHeadlessChrome spins up a headless Chrome via chromedp, skipping the test if
// no Chrome/Chromium binary is available on the host.
func newHeadlessChrome(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	chromePath, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("chrome not available: %v", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.WindowSize(1280, 900),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	// Warm up so allocator/browser errors surface here rather than mid-test.
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Skipf("failed to start headless chrome: %v", err)
	}
	cancel := func() {
		browserCancel()
		allocCancel()
	}
	return browserCtx, cancel
}

func TestSameOriginIframeTraversal(t *testing.T) {
	// Serve the host page over http so the srcdoc frame is same-origin and its
	// contentDocument is accessible to the walker.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(parentWithSameOriginIframe))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("#embed", chromedp.ByID),
		// Give the srcdoc frame a beat to parse its inner document.
		chromedp.Sleep(300*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 1. Snapshot must surface the in-frame button (mode all so it is not
	//    filtered out by the viewport-only frontier default).
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	inner := findByName(snap.Elements, "Inner Frame Button")
	if inner == nil {
		t.Fatalf("snapshot did not surface the in-frame button; got %d elements: %v", len(snap.Elements), names(snap.Elements))
	}
	if inner.Role != "button" {
		t.Fatalf("expected in-frame element role button, got %q", inner.Role)
	}

	// 2. Find must locate it by query too.
	found, err := snapshot.Find(ctx, snapshot.FindOptions{Query: "Inner Frame Button"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if findByName(found.Elements, "Inner Frame Button") == nil {
		t.Fatalf("find did not surface the in-frame button; got %v", names(found.Elements))
	}

	// 3. Resolving the ref must yield TOP-LEVEL viewport coordinates: the inner
	//    button is at frame-local (10,20)+ size 140x40, and the iframe is at
	//    top-level (120,90). So the button center in top-level space is about
	//    (120+10+70, 90+20+20) = (200,130). Resolve must reflect the frame offset,
	//    not the frame-local center (~80,40).
	box, err := snapshot.ResolveBox(ctx, inner.Ref)
	if err != nil {
		t.Fatalf("resolve box: %v", err)
	}
	if !box.OK {
		t.Fatalf("resolve box not ok: %+v", box)
	}
	// The iframe occupies x in [120,420], y in [90,290] in top-level viewport.
	// The resolved center must land inside that rectangle, which it cannot do
	// without the frame-offset translation (frame-local center would be ~(80,40)).
	if box.ViewportX < 120 || box.ViewportX > 420 {
		t.Fatalf("viewport_x %.1f not translated into iframe x-range [120,420] (frame offset missing?)", box.ViewportX)
	}
	if box.ViewportY < 90 || box.ViewportY > 290 {
		t.Fatalf("viewport_y %.1f not translated into iframe y-range [90,290] (frame offset missing?)", box.ViewportY)
	}
	// Tighter sanity: expected center near (200,130) within a generous tolerance.
	if box.ViewportX < 170 || box.ViewportX > 230 {
		t.Fatalf("viewport_x %.1f not near expected top-level center ~200", box.ViewportX)
	}
	if box.ViewportY < 110 || box.ViewportY > 150 {
		t.Fatalf("viewport_y %.1f not near expected top-level center ~130", box.ViewportY)
	}

	// 4. ResolveOrRecoverBox (the click path) must agree with ResolveBox.
	rec, err := snapshot.ResolveOrRecoverBox(ctx, inner.Ref)
	if err != nil {
		t.Fatalf("resolve-or-recover box: %v", err)
	}
	if rec.Recovered {
		t.Fatalf("did not expect ref recovery for a fresh ref")
	}
	if diffGT(rec.ViewportX, box.ViewportX, 1.5) || diffGT(rec.ViewportY, box.ViewportY, 1.5) {
		t.Fatalf("recover-path coords (%.1f,%.1f) disagree with resolve coords (%.1f,%.1f)",
			rec.ViewportX, rec.ViewportY, box.ViewportX, box.ViewportY)
	}
}

// TestCrossOriginIframeIsSafelySkipped proves the walker never throws on a
// cross-origin frame (whose contentDocument access raises a SecurityError) and
// still surfaces same-origin top-level controls.
func TestCrossOriginIframeIsSafelySkipped(t *testing.T) {
	// A second, distinct-origin server provides the cross-origin frame source.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><button id="x">Cross Origin Button</button></body></html>`))
	}))
	defer other.Close()

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0">` +
			`<button id="top">Host Button</button>` +
			`<iframe id="xframe" src="` + other.URL + `" style="width:200px;height:120px;border:0"></iframe>` +
			`</body></html>`))
	}))
	defer host.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx,
		chromedp.Navigate(host.URL),
		chromedp.WaitVisible("#xframe", chromedp.ByID),
		chromedp.Sleep(400*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Must not error/throw even though the frame is cross-origin.
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot threw on cross-origin frame: %v", err)
	}
	if findByName(snap.Elements, "Host Button") == nil {
		t.Fatalf("same-origin host button missing after cross-origin skip; got %v", names(snap.Elements))
	}
	// The cross-origin button must be absent — it is unreachable, and that's fine.
	if findByName(snap.Elements, "Cross Origin Button") != nil {
		t.Fatalf("cross-origin button should not be reachable but was surfaced")
	}
}

func findByName(els []snapshot.Element, name string) *snapshot.Element {
	for i := range els {
		if els[i].Name == name {
			return &els[i]
		}
	}
	return nil
}

func names(els []snapshot.Element) []string {
	out := make([]string, 0, len(els))
	for i := range els {
		out = append(out, els[i].Name)
	}
	return out
}

func diffGT(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d > tol
}
