package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

// oopifInnerDoc is served on a different SITE from its embedder (localhost vs
// 127.0.0.1 — Chrome's site isolation ignores the port), so Chrome puts it in its
// own renderer process and gives it a CDP target of its own. It records its own
// clicks, which is the only evidence that distinguishes a click that reached the
// frame's document from one that landed on the embedder.
const oopifInnerDoc = `<!doctype html>
<html><head><meta charset="utf-8"><title>embedded editor</title></head>
<body style="margin:0">
  <button id="go" style="position:absolute;left:10px;top:20px;width:180px;height:40px">Frame Go</button>
  <div id="log"></div>
  <script>
    document.getElementById('go').addEventListener('click', function() {
      var a = document.createElement('a');
      a.href = '/clicked';
      a.textContent = 'frame click recorded';
      document.getElementById('log').appendChild(a);
    });
  </script>
</body></html>`

// oopifDisabledInnerDoc is the same document with the control disabled. A click
// path that skips the actionability gate dispatches at its coordinates and
// reports OK, because a disabled button swallows the event silently.
const oopifDisabledInnerDoc = `<!doctype html>
<html><head><meta charset="utf-8"><title>embedded editor</title></head>
<body style="margin:0">
  <button id="go" disabled style="position:absolute;left:10px;top:20px;width:180px;height:40px">Frame Go</button>
  <div id="log"></div>
</body></html>`

// oopifDeepInnerDoc puts the control far down a document that exactly fills its
// frame, so the FRAME has nothing to scroll: the only way the point could come
// into view is the embedder scrolling, and a fixed frame denies that too.
const oopifDeepInnerDoc = `<!doctype html>
<html><head><meta charset="utf-8"><title>embedded editor</title></head>
<body style="margin:0">
  <button id="go" style="position:absolute;left:10px;top:3000px;width:180px;height:40px">Frame Go</button>
  <div id="log"></div>
  <script>
    document.getElementById('go').addEventListener('click', function() {
      var a = document.createElement('a');
      a.href = '/clicked';
      a.textContent = 'frame click recorded';
      document.getElementById('log').appendChild(a);
    });
  </script>
</body></html>`

// oopifFixtureServer serves one embedder whose iframe carries the given style,
// on a different site from the frame it embeds. The embedder records every click
// that reaches it: a click inside a nested browsing context does not, so a
// non-empty log is proof the click landed in the wrong document.
func oopifFixtureServer(t *testing.T, innerDoc, bodyStyle, iframeStyle string) string {
	t.Helper()
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(innerDoc))
	}))
	t.Cleanup(inner.Close)
	innerURL := strings.Replace(inner.URL, "127.0.0.1", "localhost", 1)

	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>frame host</title></head>
<body style="margin:0;%s">
  <button id="top" style="position:absolute;left:10px;top:10px;width:120px;height:30px">Top Button</button>
  <iframe id="embed" src="%s" style="%s;border:0"></iframe>
  <script>
    window.__topClicks = [];
    document.addEventListener('click', function(e) {
      window.__topClicks.push((e.target && e.target.id) || 'unknown');
    }, true);
  </script>
</body></html>`, bodyStyle, innerURL, iframeStyle)
	}))
	t.Cleanup(outer.Close)
	return outer.URL
}

func refNamed(elements []snapshot.Element, name string) string {
	for _, el := range elements {
		if el.Name == name {
			return el.Ref
		}
	}
	return ""
}

// newOOPIFManager launches one headless Chrome with site isolation on and a
// viewport small enough that a frame placed below the fold really is off screen.
func newOOPIFManager(t *testing.T, ctx context.Context) *Manager {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	m, err := New(ctx, Config{
		Timeout: 30 * time.Second,
		// Its own profile: the shared default one is held by whatever brw Chrome
		// is already running, and a test that skips because of that proves nothing.
		UserDataDir: t.TempDir(),
		ChromeArgs: []string{
			"--headless=new", "--disable-gpu", "--no-sandbox",
			// The frame has to land in its own process for this to be an OOPIF at all.
			"--site-per-process",
			"--window-size=900,700",
		},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestManagerClicksARefInsideACrossOriginFrame is the end-to-end acceptance for
// task Q5YB9D on the transport that supports it: brw_snapshot with
// include_frames surfaces the controls inside an out-of-process iframe as refs,
// and brw_click on one of those refs actuates the element in that frame — or
// refuses by name when it cannot.
//
// The below-the-fold case is the one that made the difference between "the ref
// resolves" and "the click lands where the ref points". Resolving translated the
// frame-local box by the frame's box and stopped there, so a frame at y=3200 in a
// 700px viewport produced a point far below the fold; CDP clamps such a point
// into the visible page, so the click was dispatched into the EMBEDDING document
// and the observation, which reads the top document, called it a success.
func TestManagerClicksARefInsideACrossOriginFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	m := newOOPIFManager(t, ctx)

	cases := []struct {
		name        string
		innerDoc    string
		bodyStyle   string
		iframeStyle string
		wantErr     string
	}{
		{
			name:        "frame inside the viewport",
			innerDoc:    oopifInnerDoc,
			iframeStyle: "position:absolute;left:100px;top:80px;width:320px;height:220px",
		},
		{
			name:        "frame below the fold",
			innerDoc:    oopifInnerDoc,
			bodyStyle:   "height:4000px",
			iframeStyle: "position:absolute;left:100px;top:3200px;width:320px;height:220px",
		},
		{
			name:        "control disabled inside the frame",
			innerDoc:    oopifDisabledInnerDoc,
			iframeStyle: "position:absolute;left:100px;top:80px;width:320px;height:220px",
			wantErr:     "not actionable",
		},
		{
			// Neither the frame nor the page can scroll the control into view, so
			// there is no point to click and the only honest answer is a refusal.
			name:        "control outside a frame nothing can scroll",
			innerDoc:    oopifDeepInnerDoc,
			iframeStyle: "position:fixed;left:100px;top:0;width:320px;height:4000px",
			wantErr:     "outside the",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outerURL := oopifFixtureServer(t, tc.innerDoc, tc.bodyStyle, tc.iframeStyle)
			opened, err := m.Open(ctx, outerURL)
			if err != nil {
				t.Fatal(err)
			}
			tabCtx := WithTabID(ctx, opened.Tab.ID)
			t.Cleanup(func() { _ = m.CloseTab(ctx, opened.Tab.ID) })
			// Give the out-of-process frame a beat to commit its own document.
			time.Sleep(700 * time.Millisecond)

			snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
			if err != nil {
				t.Fatalf("snapshot with include_frames: %v", err)
			}
			ref := refNamed(snap.Elements, "Frame Go")
			if ref == "" {
				t.Fatalf("include_frames did not surface the cross-origin frame's button; elements: %v", elementSummary(snap.Elements))
			}
			if !snapshot.IsCrossOriginElementRef(ref) {
				t.Fatalf("frame control ref %q is not namespaced as a cross-origin element ref", ref)
			}

			_, clickErr := m.Click(tabCtx, ref)
			if tc.wantErr != "" {
				if clickErr == nil {
					t.Fatalf("click %q reported success; it cannot actuate that element", ref)
				}
				if !strings.Contains(clickErr.Error(), tc.wantErr) {
					t.Fatalf("click %q failed with %v, which does not say %q", ref, clickErr, tc.wantErr)
				}
				assertEmbedderGotNoClick(t, m, tabCtx)
				return
			}
			if clickErr != nil {
				t.Fatalf("click %q: %v", ref, clickErr)
			}

			after, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
			if err != nil {
				t.Fatalf("snapshot after click: %v", err)
			}
			if refNamed(after.Elements, "frame click recorded") == "" {
				t.Fatalf("the cross-origin frame's document did not record the click; elements: %v", elementSummary(after.Elements))
			}
			assertEmbedderGotNoClick(t, m, tabCtx)
		})
	}
}

// assertEmbedderGotNoClick fails when the embedding document saw a click. It is
// the half that catches a point translated into the wrong coordinate space: the
// frame recording nothing and the embedder recording something are the same bug
// seen from two sides, and only this side distinguishes it from a click that
// simply missed.
func assertEmbedderGotNoClick(t *testing.T, m *Manager, tabCtx context.Context) {
	t.Helper()
	value, err := m.Evaluate(tabCtx, `JSON.stringify(window.__topClicks || [])`)
	if err != nil {
		t.Fatalf("read the embedder's click log: %v", err)
	}
	log, _ := value.(string)
	if log != "" && log != "[]" {
		t.Fatalf("the click reached the EMBEDDING document: %s", log)
	}
}

// TestManagerRefusesNonClickVerbsOnCrossOriginRefs is the other side of the same
// capability: direct CDP routes a CLICK into the frame and nothing else, and it
// has to say which by name rather than answering "ref not found, re-snapshot" —
// advice that can never help, because re-snapshotting mints the same ref.
func TestManagerRefusesNonClickVerbsOnCrossOriginRefs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	m := newOOPIFManager(t, ctx)
	outerURL := oopifFixtureServer(t, oopifInnerDoc, "", "position:absolute;left:100px;top:80px;width:320px;height:220px")

	opened, err := m.Open(ctx, outerURL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	time.Sleep(700 * time.Millisecond)

	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
	if err != nil {
		t.Fatalf("snapshot with include_frames: %v", err)
	}
	ref := refNamed(snap.Elements, "Frame Go")
	if ref == "" {
		t.Fatalf("include_frames did not surface the cross-origin frame's button; elements: %v", elementSummary(snap.Elements))
	}

	if _, err := m.Hover(tabCtx, ref); err == nil {
		t.Fatalf("hover on %q reported success; it cannot reach that document", ref)
	} else if !strings.Contains(err.Error(), "cross-origin iframe") {
		t.Fatalf("hover on %q failed with %v, which does not name the capability", ref, err)
	}
}

func elementSummary(elements []snapshot.Element) []string {
	out := make([]string, 0, len(elements))
	for _, el := range elements {
		out = append(out, el.Ref+"="+el.Role+":"+el.Name)
	}
	return out
}
