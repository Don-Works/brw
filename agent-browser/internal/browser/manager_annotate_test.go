package browser

import (
	"bytes"
	"context"
	"image/png"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// somSamplePath is where the test drops a human-reviewable Set-of-Marks capture.
const somSamplePath = "/tmp/agentbrowser_som_sample.png"

// visuallyRichSoMPage has a canvas drawing, an image, and several buttons so the
// annotated screenshot has labelled boxes worth eyeballing.
const visuallyRichSoMPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>SoM sample</title></head>
<body style="margin:0;padding:0;font-family:sans-serif">
  <h1 style="position:absolute;left:20px;top:8px">Agent Browser SoM</h1>
  <canvas id="chart" width="320" height="200"
          style="position:absolute;left:20px;top:60px;width:320px;height:200px;border:1px solid #ccc"></canvas>
  <img id="hero" alt="Hero"
       style="position:absolute;left:360px;top:60px;width:300px;height:200px"
       src="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20width='300'%20height='200'%3E%3Crect%20width='300'%20height='200'%20fill='%23357'/%3E%3Ctext%20x='20'%20y='110'%20fill='white'%20font-size='28'%3EHero%20Image%3C/text%3E%3C/svg%3E">
  <button id="buy" style="position:absolute;left:20px;top:290px;width:140px;height:44px">Buy now</button>
  <button id="cart" style="position:absolute;left:180px;top:290px;width:140px;height:44px">Add to cart</button>
  <a id="more" href="/more" style="position:absolute;left:340px;top:300px">Learn more</a>
  <input id="q" type="text" placeholder="Search" style="position:absolute;left:20px;top:360px;width:300px;height:30px">
  <script>
    var c = document.getElementById('chart');
    var g = c.getContext('2d');
    g.fillStyle = '#2a9d8f'; g.fillRect(20, 30, 80, 150);
    g.fillStyle = '#e76f51'; g.fillRect(120, 70, 80, 110);
    g.fillStyle = '#264653'; g.fillRect(220, 50, 80, 130);
  </script>
</body></html>`

func newSoMTab(t *testing.T) (*Manager, context.Context, func()) {
	t.Helper()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)

	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html," + url.PathEscape(visuallyRichSoMPage)).Do(rc)
		return e
	}); err != nil {
		cancel()
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	return m, ctx, cancel
}

// TestScreenshotAnnotatedProducesValidPNGAndCleansUp exercises the real injected
// overlay JS end-to-end: it captures a plain screenshot and an annotated one,
// asserts the annotated PNG is valid, non-empty, and differs from the plain one,
// asserts the legend refs are the same refs a snapshot returns, and proves the
// page is left clean (no overlay nodes, snapshot unchanged) after capture.
func TestScreenshotAnnotatedProducesValidPNGAndCleansUp(t *testing.T) {
	m, ctx, cancel := newSoMTab(t)
	defer cancel()

	plain, err := m.Screenshot(ctx)
	if err != nil {
		t.Fatalf("plain screenshot: %v", err)
	}
	if len(plain.Data) == 0 {
		t.Fatal("plain screenshot is empty")
	}

	// Snapshot before annotating so we can prove the page is unchanged after.
	before, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "frontier"})
	if err != nil {
		t.Fatalf("snapshot before: %v", err)
	}
	beforeRefs := refSet(before.Elements)

	shot, err := m.ScreenshotAnnotated(ctx, "frontier")
	if err != nil {
		t.Fatalf("annotated screenshot: %v", err)
	}
	if len(shot.Data) == 0 {
		t.Fatal("annotated screenshot is empty")
	}
	if shot.MIMEType != "image/png" {
		t.Fatalf("annotated mime = %q, want image/png", shot.MIMEType)
	}
	if _, err := png.Decode(bytes.NewReader(shot.Data)); err != nil {
		t.Fatalf("annotated screenshot is not a valid PNG: %v", err)
	}
	if bytes.Equal(shot.Data, plain.Data) {
		t.Fatal("annotated screenshot is byte-identical to the plain one (overlay not drawn)")
	}
	if len(shot.Legend) == 0 {
		t.Fatal("annotated screenshot returned an empty legend")
	}
	for ref, entry := range shot.Legend {
		if !beforeRefs[ref] {
			t.Fatalf("legend ref %q is not one of the snapshot refs (labels must match snapshot)", ref)
		}
		if entry.Width <= 0 || entry.Height <= 0 {
			t.Fatalf("legend entry %q has non-positive box: %#v", ref, entry)
		}
		if entry.Ref != ref {
			t.Fatalf("legend entry key %q != entry.Ref %q", ref, entry.Ref)
		}
	}

	// Drop a sample for human visual review.
	if err := os.WriteFile(somSamplePath, shot.Data, 0o644); err != nil {
		t.Logf("warning: could not write SoM sample to %s: %v", somSamplePath, err)
	} else {
		t.Logf("wrote SoM sample to %s (%d bytes)", somSamplePath, len(shot.Data))
	}

	// Page must be clean: no overlay nodes left behind.
	leftover, err := m.Evaluate(ctx, `document.querySelectorAll('[data-agent-browser-annotation]').length`)
	if err != nil {
		t.Fatalf("eval leftover nodes: %v", err)
	}
	if n, _ := leftover.(float64); n != 0 {
		t.Fatalf("overlay nodes left in DOM after annotated capture: %v", leftover)
	}

	// Snapshot must be unchanged (same refs) post-annotate.
	after, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "frontier"})
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	afterRefs := refSet(after.Elements)
	if len(afterRefs) != len(beforeRefs) {
		t.Fatalf("snapshot element count changed after annotate: before=%d after=%d", len(beforeRefs), len(afterRefs))
	}
	for ref := range beforeRefs {
		if !afterRefs[ref] {
			t.Fatalf("snapshot ref %q disappeared after annotate (page was mutated)", ref)
		}
	}
}

func refSet(els []snapshot.Element) map[string]bool {
	out := make(map[string]bool, len(els))
	for _, el := range els {
		out[el.Ref] = true
	}
	return out
}
