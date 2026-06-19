package snapshot_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// visualRichPage paints a <canvas>, a large <img>, a large background-image
// box, and several buttons so the visual-island detector has every island type
// to find. The canvas/img/bg box are positioned above the fold and sized over
// the detector's area thresholds.
const visualRichPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>visual rich</title></head>
<body style="margin:0;padding:0">
  <canvas id="chart" width="300" height="200" title="Sales chart"
          style="position:absolute;left:10px;top:10px;width:300px;height:200px"></canvas>
  <img id="hero" alt="Mountain hero photo"
       style="position:absolute;left:340px;top:10px;width:320px;height:240px"
       src="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20width='320'%20height='240'%3E%3Crect%20width='320'%20height='240'%20fill='%23456'/%3E%3C/svg%3E">
  <div id="bg" aria-label="Decorative banner"
       style="position:absolute;left:10px;top:260px;width:400px;height:180px;background-image:url('data:image/svg+xml,%3Csvg%20xmlns=%22http://www.w3.org/2000/svg%22%20width=%2210%22%20height=%2210%22%3E%3Crect%20width=%2210%22%20height=%2210%22%20fill=%22%23abc%22/%3E%3C/svg%3E')"></div>
  <button id="b1" style="position:absolute;left:10px;top:460px">Buy now</button>
  <button id="b2" style="position:absolute;left:120px;top:460px">Add to cart</button>
  <script>
    var c = document.getElementById('chart');
    var g = c.getContext('2d');
    g.fillStyle = '#0a0'; g.fillRect(20, 20, 120, 120);
    g.fillStyle = '#a00'; g.fillRect(160, 40, 100, 100);
  </script>
</body></html>`

func serveHTML(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVisualIslandsOffByDefault(t *testing.T) {
	srv := serveHTML(t, visualRichPage)
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
	for _, el := range snap.Elements {
		for _, s := range el.Source {
			if s == "visual" {
				t.Fatalf("visual island %q surfaced with VisualIslands off (backward-compat broken)", el.Ref)
			}
		}
	}
}

func TestVisualIslandsSurfaceCanvasAndImage(t *testing.T) {
	srv := serveHTML(t, visualRichPage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{
		Mode:          "all",
		VisualIslands: true,
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	byType := map[string]snapshot.Element{}
	for _, el := range snap.Elements {
		isVisual := false
		for _, s := range el.Source {
			if s == "visual" {
				isVisual = true
			}
		}
		if !isVisual {
			continue
		}
		if el.Ref == "" {
			t.Fatalf("visual island %#v has no ref", el)
		}
		if el.Role != "generic" {
			t.Fatalf("visual island %q role = %q, want generic", el.Ref, el.Role)
		}
		byType[el.VisualType] = el
	}

	if _, ok := byType["canvas"]; !ok {
		t.Fatalf("expected a canvas visual island; got types %v", keysOf(byType))
	}
	if img, ok := byType["image"]; !ok {
		t.Fatalf("expected an image visual island; got types %v", keysOf(byType))
	} else if img.VisualHint != "Mountain hero photo" {
		t.Fatalf("image visual_hint = %q, want %q", img.VisualHint, "Mountain hero photo")
	}
}

func TestVisualIslandsRefsStableAcrossSnapshots(t *testing.T) {
	srv := serveHTML(t, visualRichPage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	opts := snapshot.SnapshotOptions{Mode: "all", VisualIslands: true}
	first, err := snapshot.EvaluateWithOptions(ctx, opts)
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	second, err := snapshot.EvaluateWithOptions(ctx, opts)
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}

	canvasRef1 := visualRefByType(first.Elements, "canvas")
	canvasRef2 := visualRefByType(second.Elements, "canvas")
	if canvasRef1 == "" || canvasRef2 == "" {
		t.Fatalf("missing canvas ref across snapshots: %q %q", canvasRef1, canvasRef2)
	}
	if canvasRef1 != canvasRef2 {
		t.Fatalf("canvas ref changed between snapshots: %q -> %q (stable ref broken)", canvasRef1, canvasRef2)
	}
}

func keysOf(m map[string]snapshot.Element) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func visualRefByType(els []snapshot.Element, vt string) string {
	for _, el := range els {
		if el.VisualType == vt {
			return el.Ref
		}
	}
	return ""
}
