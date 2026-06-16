package snapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// visualSurfacePage exercises the visual-coverage additions: a salient named
// image must be surfaced as role "image"; a tiny unnamed icon must NOT (size
// gate); a draggable element must be surfaced (so agents can target it); and the
// native HTML5 drag protocol between two refs must fire the drop handler.
const visualSurfacePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>visual</title></head>
<body style="margin:0;padding:0">
  <img id="big" alt="Big Picture" style="width:120px;height:120px;display:block">
  <img id="icon" style="width:16px;height:16px;display:block">
  <div id="src" draggable="true" style="width:90px;height:40px">DRAG ME</div>
  <div id="dst" draggable="true" style="width:90px;height:40px">DROP HERE</div>
  <p id="log"></p>
  <script>
    var src=document.getElementById('src'), dst=document.getElementById('dst');
    src.addEventListener('dragstart', function(e){ e.dataTransfer.setData('text/plain','payload'); });
    dst.addEventListener('dragover', function(e){ e.preventDefault(); });
    dst.addEventListener('drop', function(e){ e.preventDefault();
      document.getElementById('log').textContent = 'dropped:'+e.dataTransfer.getData('text/plain'); });
  </script>
</body></html>`

func TestVisualSurfaceImagesAndDraggables(t *testing.T) {
	srv := serveHTML(t, visualSurfacePage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeHidden: true})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	var bigImg, srcRef, dstRef string
	var sawIcon bool
	for _, el := range snap.Elements {
		switch evalString(t, ctx, `document.querySelector('[data-agent-browser-ref="`+el.Ref+`"]').id`) {
		case "big":
			bigImg = el.Ref
			if el.Role != "image" {
				t.Errorf("big image surfaced with role %q, want image", el.Role)
			}
			if el.Name != "Big Picture" {
				t.Errorf("big image name %q, want Big Picture", el.Name)
			}
		case "icon":
			sawIcon = true
		case "src":
			srcRef = el.Ref
		case "dst":
			dstRef = el.Ref
		}
	}

	if bigImg == "" {
		t.Error("salient named image was not surfaced as a ref")
	}
	if sawIcon {
		t.Error("tiny unnamed icon image must NOT be surfaced (size gate)")
	}
	if srcRef == "" {
		t.Error("draggable element was not surfaced as a ref")
	}
	if dstRef == "" {
		t.Fatal("drop target not surfaced; cannot test drag")
	}

	dropped, err := snapshot.DragHtml5(ctx, srcRef, dstRef)
	if err != nil {
		t.Fatalf("DragHtml5: %v", err)
	}
	if !dropped {
		t.Error("HTML5 drag target did not accept the drop (preventDefault not observed)")
	}
	if got := evalString(t, ctx, `(document.getElementById('log')||{}).textContent`); got != "dropped:payload" {
		t.Errorf("drop handler log = %q, want dropped:payload — the shared DataTransfer must carry dragstart data", got)
	}
}
