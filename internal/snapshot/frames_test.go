package snapshot_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// frameScopeHost embeds a same-origin iframe whose document reuses the SAME ids
// as the parent. That collision is the reason frame switching exists: a bare
// selector is ambiguous across frames, and the switch is what says which one you
// mean.
const frameScopeHost = `<!doctype html>
<html><head><meta charset="utf-8"><title>frame scope host</title></head>
<body style="margin:0">
  <p id="who">main document</p>
  <button id="dup" style="position:absolute;left:10px;top:40px;width:120px;height:30px">Main Button</button>
  <iframe id="embed" style="position:absolute;left:20px;top:100px;width:320px;height:160px;border:0"
    srcdoc="<!doctype html><html><body style='margin:0'><p id='who'>inside the frame</p><button id='dup' style='position:absolute;left:10px;top:30px;width:120px;height:30px'>Frame Button</button></body></html>">
  </iframe>
</body></html>`

func openFixture(t *testing.T, body string) (context.Context, *httptest.Server) {
	t.Helper()
	srv := serveHTML(t, body)
	browserCtx, cancel := newHeadlessChrome(t)
	t.Cleanup(cancel)
	ctx, ctxCancel := context.WithTimeout(browserCtx, 40*time.Second)
	t.Cleanup(ctxCancel)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitReady("body"),
		// Give a srcdoc frame a beat to parse its own document.
		chromedp.Sleep(300*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	return ctx, srv
}

func getValue(t *testing.T, ctx context.Context, what, target, name string) any {
	t.Helper()
	var out map[string]any
	if err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.BuildGetExpression(what, target, name), &out)); err != nil {
		t.Fatalf("get %s(%s): %v", what, target, err)
	}
	return out["value"]
}

func elementNames(els []snapshot.Element) []string {
	names := make([]string, 0, len(els))
	for _, el := range els {
		if el.Name != "" {
			names = append(names, el.Name)
		}
	}
	return names
}

func containsName(els []snapshot.Element, name string) bool {
	for _, el := range els {
		if el.Name == name {
			return true
		}
	}
	return false
}

func TestFrameSwitchScopesElementLookups(t *testing.T) {
	ctx, _ := openFixture(t, frameScopeHost)

	if got := getValue(t, ctx, "text", "#who", ""); got != "main document" {
		t.Fatalf("unscoped get text #who = %v, want the main document's copy", got)
	}

	info, err := snapshot.SwitchFrame(ctx, "#embed")
	if err != nil {
		t.Fatalf("switch to #embed: %v", err)
	}
	if !info.Switched || !info.Accessible || info.Kind != "frame" {
		t.Fatalf("switch result = %+v, want a switched accessible frame", info)
	}
	if info.Scope != "#embed" {
		t.Fatalf("scope = %q, want #embed", info.Scope)
	}
	if info.ElementCount == 0 {
		t.Fatalf("switch reported %d elements in the frame, want its own document's count", info.ElementCount)
	}

	if got := getValue(t, ctx, "text", "#who", ""); got != "inside the frame" {
		t.Fatalf("scoped get text #who = %v, want the frame's copy", got)
	}
	if got := getValue(t, ctx, "count", "p", ""); got != float64(1) {
		t.Fatalf("scoped count of p = %v, want 1 (only the frame's paragraph)", got)
	}

	// The scope is not a getter feature: every walker reads its roots through it,
	// so a snapshot taken inside a frame sees only that frame's controls.
	scoped, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("scoped snapshot: %v", err)
	}
	if !containsName(scoped.Elements, "Frame Button") {
		t.Fatalf("scoped snapshot missed the frame's button; got %v", elementNames(scoped.Elements))
	}
	if containsName(scoped.Elements, "Main Button") {
		t.Fatalf("scoped snapshot still returned the parent's button; got %v", elementNames(scoped.Elements))
	}

	back, err := snapshot.SwitchFrame(ctx, "main")
	if err != nil {
		t.Fatalf("switch back to main: %v", err)
	}
	if back.Scope != "main" || back.Kind != "main" || !back.Switched {
		t.Fatalf("switch back result = %+v, want the main document", back)
	}
	if got := getValue(t, ctx, "text", "#who", ""); got != "main document" {
		t.Fatalf("get text #who after frame main = %v, want the main document's copy", got)
	}
	full, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("unscoped snapshot: %v", err)
	}
	if !containsName(full.Elements, "Main Button") || !containsName(full.Elements, "Frame Button") {
		t.Fatalf("snapshot after frame main = %v, want both documents' buttons", elementNames(full.Elements))
	}
}

// A ref an agent already holds points at an element, not at a frame. Passing one
// selects the frame that contains it, which is what makes "switch to where this
// thing lives" expressible.
func TestFrameSwitchAcceptsARefInsideTheFrame(t *testing.T) {
	ctx, _ := openFixture(t, frameScopeHost)

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, el := range snap.Elements {
		if el.Name == "Frame Button" {
			ref = el.Ref
		}
	}
	if ref == "" {
		t.Fatalf("no ref for the in-frame button; got %v", elementNames(snap.Elements))
	}

	info, err := snapshot.SwitchFrame(ctx, ref)
	if err != nil {
		t.Fatalf("switch to the frame holding %s: %v", ref, err)
	}
	if !info.Switched || !info.Accessible {
		t.Fatalf("switch result = %+v, want the containing frame", info)
	}
	if got := getValue(t, ctx, "text", "#who", ""); got != "inside the frame" {
		t.Fatalf("scoped get text #who = %v, want the frame's copy", got)
	}
}

// TestFrameSwitchResolvesCrossOriginFrame drives the f<i> refs the frame walk
// mints for frames the browser isolates. There is nothing to scope into, so the
// switch reports the frame's top-level box and origin and leaves the scope
// alone — the coordinate fallback, named rather than silent.
func TestFrameSwitchResolvesCrossOriginFrame(t *testing.T) {
	other := serveHTML(t, `<!doctype html><html><body><button id="x">Cross Origin Button</button></body></html>`)
	ctx, _ := openFixture(t, `<!doctype html><html><body style="margin:0">`+
		`<p id="who">main document</p>`+
		`<iframe id="xframe" src="`+other.URL+`" style="position:absolute;left:30px;top:60px;width:220px;height:140px;border:0"></iframe>`+
		`</body></html>`)

	// The same walk that feeds the snapshot's cross_origin_frames metadata is
	// what assigns f0, so the two have to agree.
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	frames, _ := snap.Metadata["cross_origin_frames"].([]any)
	if len(frames) == 0 {
		t.Fatalf("no cross_origin_frames metadata; got %v", snap.Metadata)
	}

	for _, target := range []string{"f0", "#xframe"} {
		t.Run(target, func(t *testing.T) {
			info, err := snapshot.SwitchFrame(ctx, target)
			if err != nil {
				t.Fatalf("switch to %s: %v", target, err)
			}
			if info.Kind != "cross_origin" {
				t.Fatalf("kind = %q, want cross_origin", info.Kind)
			}
			if info.Switched || info.Accessible {
				t.Fatalf("switch result = %+v, want switched:false accessible:false for an isolated frame", info)
			}
			if info.Origin != other.URL {
				t.Fatalf("origin = %q, want the embedded server's origin %q", info.Origin, other.URL)
			}
			if info.Width <= 0 || info.Height <= 0 {
				t.Fatalf("box = %.0fx%.0f at (%.0f,%.0f), want the frame's top-level box", info.Width, info.Height, info.X, info.Y)
			}
			if !strings.Contains(info.Note, "brw_click_xy") {
				t.Fatalf("note = %q, want it to name the coordinate fallback", info.Note)
			}
		})
	}

	// The scope was never moved, so the parent document is still readable.
	if got := getValue(t, ctx, "text", "#who", ""); got != "main document" {
		t.Fatalf("get text #who after a cross-origin switch = %v, want the main document", got)
	}
	if _, err := snapshot.SwitchFrame(ctx, "f7"); err == nil {
		t.Fatal("an f<i> ref with no matching frame should be an error, not a silent no-op")
	}
}

func TestFrameSwitchRejectsUnknownTarget(t *testing.T) {
	ctx, _ := openFixture(t, frameScopeHost)
	if _, err := snapshot.SwitchFrame(ctx, "#not-a-frame"); err == nil {
		t.Fatal("switching to a selector that matches nothing should fail")
	}
}

// A scope whose frame has gone away must fail loudly. Silently widening back to
// the whole document would answer a scoped question from the wrong frame, which
// is the exact failure the switch exists to prevent.
func TestFrameScopeGoesStaleLoudly(t *testing.T) {
	ctx, _ := openFixture(t, frameScopeHost)

	if _, err := snapshot.SwitchFrame(ctx, "#embed"); err != nil {
		t.Fatalf("switch to #embed: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('embed').remove()`, nil)); err != nil {
		t.Fatalf("remove the frame: %v", err)
	}
	var out map[string]any
	err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.BuildGetExpression("text", "#who", ""), &out))
	if err == nil {
		t.Fatalf("a get against a removed frame scope returned %v, want an error", out)
	}
	if !strings.Contains(err.Error(), "no longer resolves") {
		t.Fatalf("error %q should say the frame scope no longer resolves", err)
	}
	if _, err := snapshot.SwitchFrame(ctx, "main"); err != nil {
		t.Fatalf("frame main must always be a way out: %v", err)
	}
	if got := getValue(t, ctx, "text", "#who", ""); got != "main document" {
		t.Fatalf("get text #who after recovering = %v, want the main document", got)
	}
}
