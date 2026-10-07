package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

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

const oopifDisabledInnerDoc = `<!doctype html>
<html><head><meta charset="utf-8"><title>embedded editor</title></head>
<body style="margin:0">
  <button id="go" disabled style="position:absolute;left:10px;top:20px;width:180px;height:40px">Frame Go</button>
  <div id="log"></div>
</body></html>`

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

func newOOPIFManager(t *testing.T, ctx context.Context) *Manager {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	profile := browsertest.NewProfile(t)
	m, err := New(ctx, Config{
		Timeout: 30 * time.Second,

		UserDataDir: profile.Dir(),
		ChromeArgs: []string{
			"--headless=new", "--disable-gpu", "--no-sandbox",

			"--site-per-process",
			"--window-size=900,700",
		},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	profile.StopWith(func() { _ = m.Close() })
	return m
}

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

			var snap snapshot.PageSnapshot
			var ref string
			for deadline := time.Now().Add(15 * time.Second); ; {
				snap, err = m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
				if err != nil {
					t.Fatalf("snapshot with include_frames: %v", err)
				}
				if ref = refNamed(snap.Elements, "Frame Go"); ref != "" || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
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

func TestClickIntoACrossOriginFrameAsksAboutTheFramesOwnOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	m := newOOPIFManager(t, ctx)
	outerURL := oopifFixtureServer(t, oopifInnerDoc, "", "position:absolute;left:100px;top:80px;width:320px;height:220px")

	opened, err := m.Open(ctx, outerURL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	t.Cleanup(func() { _ = m.CloseTab(ctx, opened.Tab.ID) })
	time.Sleep(700 * time.Millisecond)

	allowCtx := WithFrameReadCheck(tabCtx, func(string) error { return nil })
	snap, err := m.Snapshot(allowCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
	if err != nil {
		t.Fatalf("snapshot with include_frames: %v", err)
	}
	ref := refNamed(snap.Elements, "Frame Go")
	if ref == "" {
		t.Fatalf("include_frames did not surface the cross-origin frame's button; elements: %v", elementSummary(snap.Elements))
	}

	var asked []string
	refuseCtx := WithFrameActCheck(tabCtx, func(origin string) error {
		asked = append(asked, origin)
		return errors.New("no site permission grant for " + origin + " (scope act)")
	})
	_, clickErr := m.Click(refuseCtx, ref)
	if clickErr == nil {
		t.Fatalf("click %q actuated a document nobody decided about", ref)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "localhost") {
		t.Fatalf("the gate was asked %v; the click has to be checked against the origin the FRAME is serving, not the tab's", asked)
	}
	if !strings.Contains(clickErr.Error(), asked[0]) || !strings.Contains(clickErr.Error(), "no site permission grant") {
		t.Fatalf("the refusal does not say which origin was refused or why: %v", clickErr)
	}

	refused, err := m.Snapshot(allowCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
	if err != nil {
		t.Fatalf("snapshot after the refused click: %v", err)
	}
	if refNamed(refused.Elements, "frame click recorded") != "" {
		t.Fatalf("the refused click actuated the frame anyway; elements: %v", elementSummary(refused.Elements))
	}
	assertEmbedderGotNoClick(t, m, tabCtx)

	if _, err := m.Click(WithFrameActCheck(allowCtx, func(string) error { return nil }), ref); err != nil {
		t.Fatalf("click %q with the frame's origin granted: %v", ref, err)
	}
	after, err := m.Snapshot(allowCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
	if err != nil {
		t.Fatalf("snapshot after click: %v", err)
	}
	if refNamed(after.Elements, "frame click recorded") == "" {
		t.Fatalf("the granted click did not reach the frame's document; elements: %v", elementSummary(after.Elements))
	}
}

func TestOpenDoesNotStrandACrossSiteFrameThatLoadsDuringTheNavigation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m := newOOPIFManager(t, ctx)

	served := make(chan struct{}, 1)
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(oopifInnerDoc))
		select {
		case served <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(inner.Close)
	innerURL := strings.Replace(inner.URL, "127.0.0.1", "localhost", 1)

	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>frame host</title></head><body>
<iframe id="embed" src="%s" style="position:absolute;left:100px;top:80px;width:320px;height:220px;border:0"></iframe>
`, innerURL)
		w.(http.Flusher).Flush()
		select {
		case <-served:
			time.Sleep(300 * time.Millisecond)
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `</body></html>`)
	}))
	t.Cleanup(outer.Close)

	opened, err := m.Open(ctx, outer.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	t.Cleanup(func() { _ = m.CloseTab(ctx, opened.Tab.ID) })

	var snap snapshot.PageSnapshot
	for deadline := time.Now().Add(10 * time.Second); ; {
		snap, err = m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: true})
		if err != nil {
			t.Fatalf("snapshot with include_frames: %v", err)
		}
		if refNamed(snap.Elements, "Frame Go") != "" {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the cross-site frame never committed its document; elements: %v", elementSummary(snap.Elements))
}
