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

func oopifFixtureServer(t *testing.T) string {
	t.Helper()
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(oopifInnerDoc))
	}))
	t.Cleanup(inner.Close)
	innerURL := strings.Replace(inner.URL, "127.0.0.1", "localhost", 1)

	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>frame host</title></head>
<body style="margin:0">
  <button id="top" style="position:absolute;left:10px;top:10px;width:120px;height:30px">Top Button</button>
  <iframe id="embed" src="%s" style="position:absolute;left:100px;top:80px;width:320px;height:220px;border:0"></iframe>
</body></html>`, innerURL)
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

// TestManagerClicksARefInsideACrossOriginFrame is the end-to-end acceptance for
// task Q5YB9D on the transport that supports it: brw_snapshot with
// include_frames surfaces the controls inside an out-of-process iframe as refs,
// and brw_click on one of those refs actuates the element in that frame.
//
// Deleting either half fails it: without the frame read there is no f<i>: ref to
// click, and without the routing in Click the ref reaches the top document's
// resolver and is refused.
func TestManagerClicksARefInsideACrossOriginFrame(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	outerURL := oopifFixtureServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	m, err := New(ctx, Config{
		Timeout: 30 * time.Second,
		// Its own profile: the shared default one is held by whatever brw Chrome
		// is already running, and a test that skips because of that proves nothing.
		UserDataDir: t.TempDir(),
		ChromeArgs: []string{
			"--headless=new", "--disable-gpu", "--no-sandbox",
			// The frame has to land in its own process for this to be an OOPIF at all.
			"--site-per-process",
		},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	defer m.Close()

	opened, err := m.Open(ctx, outerURL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
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

	if _, err := m.Click(tabCtx, ref); err != nil {
		t.Fatalf("click %q: %v", ref, err)
	}

	after, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
	if err != nil {
		t.Fatalf("snapshot after click: %v", err)
	}
	if refNamed(after.Elements, "frame click recorded") == "" {
		t.Fatalf("the cross-origin frame's document did not record the click; elements: %v", elementSummary(after.Elements))
	}
}

// TestManagerRefusesNonClickVerbsOnCrossOriginRefs is the other side of the same
// capability: direct CDP routes a CLICK into the frame and nothing else, and it
// has to say which by name rather than answering "ref not found, re-snapshot" —
// advice that can never help, because re-snapshotting mints the same ref.
func TestManagerRefusesNonClickVerbsOnCrossOriginRefs(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	outerURL := oopifFixtureServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	m, err := New(ctx, Config{
		Timeout:     30 * time.Second,
		UserDataDir: t.TempDir(),
		ChromeArgs:  []string{"--headless=new", "--disable-gpu", "--no-sandbox", "--site-per-process"},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	defer m.Close()

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
