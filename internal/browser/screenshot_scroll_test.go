package browser

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestScreenshotsAfterDocumentScroll(t *testing.T) {
	m := newHeadlessManager(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><style>html,body{margin:0}body{width:3200px;height:4000px;background:red}section{position:absolute;left:1200px;top:1600px;width:2000px;height:2400px;background:lime}button{position:absolute;left:100px;top:100px;width:300px;height:120px;background:yellow;border:0}</style><section><button>TARGET</button></section></html>`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(ctx, `(async()=>{scrollTo(1200,1600);await new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r)));return true})()`); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitFor(ctx, "fn:scrollX === 1200 && scrollY === 1600", time.Second); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	ref := ""
	for _, e := range snap.Elements {
		if e.Role == "button" && e.Name == "TARGET" {
			ref = e.Ref
		}
	}
	if ref == "" {
		t.Fatal("missing button ref")
	}
	assertPixel := func(t *testing.T, data []byte, yellow bool) {
		t.Helper()
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		bounds := img.Bounds()
		x, y := bounds.Max.X-25, bounds.Max.Y-25
		if yellow {
			x, y = bounds.Min.X+40, bounds.Min.Y+40
		}
		r, g, b, _ := img.At(x, y).RGBA()
		if g < 55000 || b > 8000 || (!yellow && r > 8000) || (yellow && r < 55000) {
			t.Fatalf("pixel (%d,%d) = (%d,%d,%d), yellow=%t, size=%v", x, y, r, g, b, yellow, bounds)
		}
	}
	t.Run("viewport", func(t *testing.T) {
		shot, err := m.Screenshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertPixel(t, shot.Data, false)
	})
	for _, tc := range []struct {
		name   string
		opts   AnnotatedScreenshotOptions
		yellow bool
	}{
		{name: "annotated viewport"},
		{name: "annotated region", opts: AnnotatedScreenshotOptions{Region: ScreenshotRegion{X: 500, Y: 300, Width: 200, Height: 100}}},
		{name: "annotated ref", opts: AnnotatedScreenshotOptions{Ref: ref}, yellow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shot, err := m.ScreenshotAnnotated(ctx, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			assertPixel(t, shot.Data, tc.yellow)
			if tc.opts.Region.IsZero() {
				entry, ok := shot.Legend[ref]
				matches, err := m.Evaluate(ctx, fmt.Sprintf(`(()=>{const r=document.querySelector('button').getBoundingClientRect();return r.x===%v && r.y===%v})()`, entry.X, entry.Y))
				if !ok || err != nil || matches != true {
					t.Fatalf("legend must stay in viewport coordinates: %+v", shot.Legend)
				}
			}
		})
	}
	t.Run("element", func(t *testing.T) {
		shot, err := m.ScreenshotElement(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		assertPixel(t, shot.Data, true)
	})
}
