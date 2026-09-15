package snapshot_test

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// frameClickHost (click_text_frames_test.go) puts a button in a same-origin
// iframe whose FRAME-LOCAL centre (70,45) lands inside the main document's own
// button, and whose TOP-LEVEL centre is (90,145). A script that measures the
// frame's element and then hit-tests the top document at that point acts on the
// wrong element and reports success — which is exactly what the mouse scripts
// would do now that they can reach a ref inside a frame at all.
//
// The three scripts used to carry a shadow-only root walk, so a ref inside an
// iframe was simply "not found" there; they resolve through the shared frame-aware
// lookup now, and this is what holds them to reporting the point in the same
// space every other brw result uses.
func TestMouseScriptsResolveFrameRefsInTopLevelSpace(t *testing.T) {
	ctx, _ := openFixture(t, frameClickHost)

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var ref string
	for _, el := range snap.Elements {
		if el.Name == "Frame Button" {
			ref = el.Ref
		}
	}
	if ref == "" {
		t.Fatalf("the frame's button is not in the snapshot; names: %v", elementNames(snap.Elements))
	}
	refJSON, _ := json.Marshal(ref)

	t.Run("mouse event", func(t *testing.T) {
		var result map[string]any
		expr := fmt.Sprintf("%s({\"ref\":%s,\"button\":\"left\"})", snapshot.MouseEventScript, refJSON)
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
			t.Fatalf("mouse event on %q: %v", ref, err)
		}
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("mouse event on a ref inside a same-origin iframe failed: %v", result)
		}
		assertTopLevelPoint(t, "mouse event", result["x"], result["y"])
	})

	t.Run("drag", func(t *testing.T) {
		var result map[string]any
		expr := fmt.Sprintf("%s({\"from\":{\"ref\":%s},\"to\":{\"x\":300,\"y\":300},\"steps\":2})", snapshot.DragScript, refJSON)
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
			t.Fatalf("drag from %q: %v", ref, err)
		}
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("drag from a ref inside a same-origin iframe failed: %v", result)
		}
		from, _ := result["from"].(map[string]any)
		if from == nil {
			t.Fatalf("drag did not report its source point: %v", result)
		}
		// Drag hit-tests the TOP document at this point to pick its source
		// element, so a frame-local value here starts the drag on the main
		// document's button instead.
		assertTopLevelPoint(t, "drag source", from["x"], from["y"])
	})
}

func assertTopLevelPoint(t *testing.T, label string, rawX, rawY any) {
	t.Helper()
	x, _ := rawX.(float64)
	y, _ := rawY.(float64)
	if math.Abs(x-90) > 2 || math.Abs(y-145) > 2 {
		t.Fatalf("%s reported (%v,%v); the top-level centre is (90,145) and the frame-local one is (70,45)", label, x, y)
	}
}
