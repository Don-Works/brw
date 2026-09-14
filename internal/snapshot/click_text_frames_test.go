package snapshot_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// frameClickHost places a button in the main document and another inside a
// same-origin iframe, positioned so the FRAME-LOCAL centre of the embedded
// button lands inside the main document's button. Both documents record what
// they were clicked on, which is the only way to tell a click that reached the
// frame from one that hit the parent page and reported the frame's name.
//
// Main button: 10,40 120x30 -> top-level centre 70,55.
// Frame at 20,100; frame button 10,30 120x30 -> frame-local centre 70,45,
// which is inside the main button, and top-level centre 90,145.
const frameClickHost = `<!doctype html>
<html><head><meta charset="utf-8"><title>frame click host</title>
<script>
window.__clicked = [];
document.addEventListener('click', function(e){ window.__clicked.push('main:' + (e.target.textContent || '').trim()); }, true);
</script>
</head>
<body style="margin:0">
  <button id="dup" style="position:absolute;left:10px;top:40px;width:120px;height:30px">Main Button</button>
  <iframe id="embed" style="position:absolute;left:20px;top:100px;width:320px;height:160px;border:0"
    srcdoc="<!doctype html><html><body style='margin:0'><button id='dup' style='position:absolute;left:10px;top:30px;width:120px;height:30px'>Frame Button</button><script>document.addEventListener('click', function(e){ parent.__clicked.push('frame:' + (e.target.textContent || '').trim()); }, true);</script></body></html>">
  </iframe>
</body></html>`

func clickLog(t *testing.T, ctx context.Context) []string {
	t.Helper()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.__clicked)`, &raw)); err != nil {
		t.Fatalf("read click log: %v", err)
	}
	var log []string
	if err := json.Unmarshal([]byte(raw), &log); err != nil {
		t.Fatalf("decode click log %q: %v", raw, err)
	}
	return log
}

// TestClickTextActuatesInsideTheFrameThatHoldsTheText: brw_click_text searches
// every same-origin frame, but it used to measure the match with the frame's own
// getBoundingClientRect and then hit-test that point against the TOP document.
// A match inside an iframe therefore clicked whatever the parent page happens to
// have at those coordinates and reported the click as a success, and the point
// handed back for CDP actuation (the path a held modifier always takes) pointed
// at the same wrong place.
func TestClickTextActuatesInsideTheFrameThatHoldsTheText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		locate   bool
		wantLog  []string
		wantText string
		wantX    float64
		wantY    float64
	}{
		{
			name:     "main document match",
			text:     "Main Button",
			wantLog:  []string{"main:Main Button"},
			wantText: "Main Button",
			wantX:    70,
			wantY:    55,
		},
		{
			name:     "same-origin frame match",
			text:     "Frame Button",
			wantLog:  []string{"frame:Frame Button"},
			wantText: "Frame Button",
			wantX:    90,
			wantY:    145,
		},
		{
			// The deferred path reports a point for the caller to actuate with
			// CDP input, which only ever speaks top-level viewport coordinates.
			name:     "frame match located for a trusted dispatch",
			text:     "Frame Button",
			locate:   true,
			wantLog:  nil,
			wantText: "Frame Button",
			wantX:    90,
			wantY:    145,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := openFixture(t, frameClickHost)

			result, err := snapshot.ClickText(ctx, snapshot.ClickTextOptions{Text: tt.text, Exact: true, Locate: tt.locate})
			if err != nil {
				t.Fatalf("click text %q: %v", tt.text, err)
			}
			if result.Text != tt.wantText {
				t.Fatalf("click text %q reported text %q, want %q", tt.text, result.Text, tt.wantText)
			}
			if result.Deferred != tt.locate {
				t.Fatalf("click text %q reported deferred %v, want %v", tt.text, result.Deferred, tt.locate)
			}
			if math.Abs(result.X-tt.wantX) > 2 || math.Abs(result.Y-tt.wantY) > 2 {
				t.Fatalf("click text %q reported the point (%v,%v), want the top-level centre (%v,%v)", tt.text, result.X, result.Y, tt.wantX, tt.wantY)
			}

			log := clickLog(t, ctx)
			if len(log) != len(tt.wantLog) {
				t.Fatalf("click text %q produced clicks %v, want %v", tt.text, log, tt.wantLog)
			}
			for i, want := range tt.wantLog {
				if log[i] != want {
					t.Fatalf("click text %q produced clicks %v, want %v", tt.text, log, tt.wantLog)
				}
			}
		})
	}
}
