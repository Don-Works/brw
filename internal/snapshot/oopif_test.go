package snapshot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	brwcdp "github.com/Don-Works/brw/internal/cdp"
	"github.com/chromedp/chromedp"
)

// The fixture is two ORIGINS on two SITES. httptest binds 127.0.0.1, and Chrome's
// site isolation keys on the site (scheme + registrable host), not the port — so
// two httptest servers on 127.0.0.1 are cross-ORIGIN but share a renderer
// process, and the embedded frame gets no CDP target of its own. Addressing the
// inner server as "localhost" makes it a different SITE, which is what puts it
// out of process: exactly the frame Stagehand v4 treats as top-level content and
// the frame this file is about.
const innerFrameDoc = `<!doctype html>
<html><head><meta charset="utf-8"><title>embedded editor</title></head>
<body style="margin:0">
  <button id="go" style="position:absolute;left:10px;top:20px;width:180px;height:40px">Frame Go</button>
  <input id="title" name="frame-title" style="position:absolute;left:10px;top:80px;width:180px;height:24px">
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

func crossOriginFixture(t *testing.T, innerDoc string) (context.Context, string) {
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
<body style="margin:0">
  <button id="top" name="top-button" style="position:absolute;left:10px;top:10px;width:120px;height:30px">Top Button</button>
  <iframe id="embed" src="%s" style="position:absolute;left:100px;top:80px;width:320px;height:220px;border:0"></iframe>
  <script>
    window.__topClicks = [];
    document.addEventListener('click', function(e) {
      window.__topClicks.push((e.target && e.target.id) || 'unknown');
    }, true);
  </script>
</body></html>`, innerURL)
	}))
	t.Cleanup(outer.Close)

	chromePath, err := brwcdp.FindChrome("")
	if err != nil {
		t.Skipf("chrome not available: %v", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		// Explicit rather than relying on the default: the whole point of the
		// fixture is that the frame lands in its own process.
		chromedp.Flag("site-per-process", true),
		chromedp.WindowSize(1280, 900),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Skipf("failed to start headless chrome: %v", err)
	}
	t.Cleanup(func() {
		browserCancel()
		allocCancel()
	})
	ctx, ctxCancel := context.WithTimeout(browserCtx, 90*time.Second)
	t.Cleanup(ctxCancel)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(outer.URL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	return ctx, innerURL
}

func frameSnapshotFor(t *testing.T, ctx context.Context) []OutOfProcessFrame {
	t.Helper()
	frames, err := SnapshotOutOfProcessFrames(ctx, SnapshotOptions{Mode: "all"}, nil)
	if err != nil {
		t.Fatalf("snapshot out-of-process frames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected one out-of-process frame, got %d — the fixture's iframe did not get its own target", len(frames))
	}
	return frames
}

// TestCrossOriginFrameReadAsksAboutEachEmbeddedOrigin is the direct-CDP half of
// the consent gate include_frames needs.
//
// The snapshot was authorized against the origin the TAB is showing. Attaching a
// session to an embedded frame's own target and running the walker there is a
// read of a THIRD PARTY's document, which that grant never covered — so the
// frame's origin is put to the gate on its own, and a refused frame is not read
// at all rather than read and then discarded.
func TestCrossOriginFrameReadAsksAboutEachEmbeddedOrigin(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	var asked []string
	refuse := func(origin string) error {
		asked = append(asked, origin)
		return errors.New("no site permission grant for " + origin + " (scope read)")
	}
	frames, err := SnapshotOutOfProcessFrames(ctx, SnapshotOptions{Mode: "all"}, refuse)
	if err != nil {
		t.Fatalf("snapshot out-of-process frames: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("a refused origin was read anyway: %d frames came back", len(frames))
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "localhost") {
		t.Fatalf("the gate was asked about %v, want the embedded cross-site origin", asked)
	}

	// And the same page with the gate allowing it reads exactly as before, so the
	// gate is what decided rather than the read having quietly broken.
	allowed, err := SnapshotOutOfProcessFrames(ctx, SnapshotOptions{Mode: "all"}, func(string) error { return nil })
	if err != nil || len(allowed) != 1 {
		t.Fatalf("an allowed origin was not read: %v (%d frames)", err, len(allowed))
	}
}

// TestForgedFrameStampRefusesToBindAFrameRef covers the one input to the frame
// mapping that the PAGE can write.
//
// data-brw-xframe lives in page-writable DOM, and the walker only re-stamps
// frames it classified as inaccessible — so a stamp the page put on a frame the
// walker walks INTO survives every later walk. Two nodes claiming index 0 used to
// be resolved by DOM order, which binds f0 to whichever came first while its
// coordinates come from the other frame's box: a click on a ref from the payment
// iframe dispatched at the advert's pixels. There is no right guess here, so the
// walk refuses.
func TestForgedFrameStampRefusesToBindAFrameRef(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	if _, err := frameHandles(ctx); err != nil {
		t.Fatalf("the unforged page does not map its frames: %v", err)
	}

	// A same-origin iframe the walker enters (and therefore never stamps), wearing
	// the real frame's index.
	forge := `(function(){
		var f = document.createElement('iframe');
		f.setAttribute('data-brw-xframe', '0');
		f.setAttribute('src', 'about:blank');
		f.style.cssText = 'position:absolute;left:600px;top:10px;width:100px;height:60px';
		document.body.insertBefore(f, document.body.firstChild);
		return true;
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(forge, nil), chromedp.Sleep(200*time.Millisecond)); err != nil {
		t.Fatalf("forge a frame stamp: %v", err)
	}

	handles, err := frameHandles(ctx)
	if err == nil {
		t.Fatalf("a page carrying two elements stamped f0 still produced %d frame handles; f0 was bound by DOM order", len(handles))
	}
	if !strings.Contains(err.Error(), "f0") {
		t.Fatalf("the refusal does not name the frame it will not bind: %v", err)
	}
	// And the ref paths refuse with it rather than acting on the wrong frame.
	if _, err := ResolveCrossOriginActionPoint(ctx, "f0:e1", 0, nil); err == nil {
		t.Fatal("a ref resolved against a page whose frame stamps are ambiguous")
	}
}

func refForName(elements []Element, name string) string {
	for _, el := range elements {
		if el.Name == name {
			return el.Ref
		}
	}
	return ""
}

// TestCrossOriginFrameRefResolvesAndClicksInsideTheFrame is the acceptance for
// addressing an out-of-process iframe as content rather than as a box: a ref
// minted inside the frame resolves through a session attached to that frame's own
// CDP target, and clicking it actuates the element IN THAT FRAME.
//
// The inner document records its own click, so a click that landed on the
// embedder (or nowhere) cannot pass: the record only exists if the event reached
// the frame's document. The top document records its clicks too, and must stay
// empty — a click inside a nested browsing context does not reach it.
func TestCrossOriginFrameRefResolvesAndClicksInsideTheFrame(t *testing.T) {
	ctx, innerURL := crossOriginFixture(t, innerFrameDoc)

	snap, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("top snapshot: %v", err)
	}
	if boxes := crossOriginBoxesFromMetadata(snap.Metadata); len(boxes) != 1 {
		t.Fatalf("top walk should record one unreadable cross-origin frame, got %d", len(boxes))
	}

	frames := frameSnapshotFor(t, ctx)
	if !strings.Contains(frames[0].URL, "localhost") {
		t.Fatalf("frame snapshot came from %q, want the cross-site inner document %q", frames[0].URL, innerURL)
	}
	appended, read := MergeOutOfProcessFrames(&snap, frames)
	if appended == 0 {
		t.Fatalf("no controls merged from the cross-origin frame")
	}
	if !read[0] {
		t.Fatalf("frame 0 not reported as read: %v", read)
	}

	ref := refForName(snap.Elements, "Frame Go")
	if ref == "" {
		t.Fatalf("the frame's button is not in the merged element list; refs: %v", frameRefsOf(snap.Elements))
	}
	if !strings.HasPrefix(ref, "f0:") {
		t.Fatalf("frame control ref %q is not namespaced by its frame", ref)
	}
	if !IsCrossOriginElementRef(ref) {
		t.Fatalf("ref %q is not recognised as a cross-origin element ref", ref)
	}

	box, err := ResolveCrossOriginActionPoint(ctx, ref, 0, nil)
	if err != nil {
		t.Fatalf("resolve %q: %v", ref, err)
	}
	if !box.OK {
		t.Fatalf("resolve %q reported no visible box: %+v", ref, box)
	}
	// The iframe sits at (100,80) and the button at (10,20) 180x40 inside it, so
	// the top-level centre is (100+10+90, 80+20+20) = (200, 120). A resolver that
	// forgot to translate would report the frame-local (100, 40).
	if math.Abs(box.ViewportX-200) > 3 || math.Abs(box.ViewportY-120) > 3 {
		t.Fatalf("resolved point (%v,%v) is not the top-level centre (200,120)", box.ViewportX, box.ViewportY)
	}

	if err := chromedp.Run(ctx, chromedp.MouseClickXY(box.ViewportX, box.ViewportY)); err != nil {
		t.Fatalf("click at the resolved point: %v", err)
	}

	after := frameSnapshotFor(t, ctx)
	if refForName(after[0].Snapshot.Elements, "frame click recorded") == "" {
		t.Fatalf("the frame's own document did not record the click; its elements: %v", frameNamesOf(after[0].Snapshot.Elements))
	}

	var topClicks []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__topClicks`, &topClicks)); err != nil {
		t.Fatalf("read top click log: %v", err)
	}
	if len(topClicks) != 0 {
		t.Fatalf("the click reached the embedding document too: %v", topClicks)
	}
}

// TestFrameSwitchResolvesEveryShapeOfCrossOriginRef keeps brw_frame able to
// resolve the refs brw_snapshot actually mints.
//
// The element half of an f<i>:<ref> is a ref from the frame's own walk, and the
// walker's collision pass appends name suffixes to those (e6_edit_2). A pattern
// that only accepted f<i>:e<digits> matched the simple case, sent the suffixed
// one down the CSS-selector path, and answered "no frame matched" for a ref the
// tool description says identifies its frame.
func TestFrameSwitchResolvesEveryShapeOfCrossOriginRef(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	for _, target := range []string{"f0", "f0:e1", "f0:e6_edit_2"} {
		info, err := SwitchFrame(ctx, target)
		if err != nil {
			t.Fatalf("switch to %q: %v", target, err)
		}
		if info.Kind != "cross_origin" || info.Ref != "f0" {
			t.Fatalf("switch to %q reported %+v, want the cross-origin frame f0", target, info)
		}
		if info.Switched {
			t.Fatalf("switch to %q claimed to scope into an isolated document", target)
		}
		if info.Width <= 0 || info.Height <= 0 {
			t.Fatalf("switch to %q reported no box to act on: %+v", target, info)
		}
	}
}

// TestFrameWalkDoesNotMutateTheDocumentItAlreadyStamped guards a spin the frame
// stamp would otherwise introduce.
//
// setAttribute queues a mutation record even when the new value equals the old
// one, and the frame walk runs on EVERY poll of an in-page wait. An
// unconditional stamp therefore makes each poll wake the MutationObserver that
// schedules the next poll — a wait on any page carrying a cross-origin iframe
// would spin the renderer until its timeout.
func TestFrameWalkDoesNotMutateTheDocumentItAlreadyStamped(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	// First walk: stamps the frame element.
	if err := chromedp.Run(ctx, chromedp.Evaluate(CrossOriginFrameBoxesScript, nil)); err != nil {
		t.Fatalf("first walk: %v", err)
	}
	observe := `(function(){
		window.__xframeMutations = 0;
		new MutationObserver(function(list){
			for (var i = 0; i < list.length; i++) {
				if (list[i].attributeName === 'data-brw-xframe') window.__xframeMutations++;
			}
		}).observe(document.documentElement, {subtree: true, attributes: true});
		return true;
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(observe, nil)); err != nil {
		t.Fatalf("install observer: %v", err)
	}
	// Second walk: the stamp is already correct, so it must write nothing.
	if err := chromedp.Run(ctx, chromedp.Evaluate(CrossOriginFrameBoxesScript, nil)); err != nil {
		t.Fatalf("second walk: %v", err)
	}
	var mutations int
	// MutationObserver callbacks land at the next microtask checkpoint, so give
	// the page a tick before reading the count.
	if err := chromedp.Run(ctx,
		chromedp.Sleep(100*time.Millisecond),
		chromedp.Evaluate(`window.__xframeMutations`, &mutations),
	); err != nil {
		t.Fatalf("read mutation count: %v", err)
	}
	if mutations != 0 {
		t.Fatalf("a repeat frame walk produced %d attribute mutations; every in-page wait poll would wake its own observer", mutations)
	}
}

// TestCrossOriginFrameSessionTeardownLeavesTheTabAlone guards the sharpest edge
// in attaching to a subframe target: chromedp's context teardown calls
// Target.closeTarget on whatever it attached to, and closing a SUBFRAME host
// closes the whole tab. So a tab operation that hits its deadline while a frame
// read is in flight would close the page it was reading.
//
// The test cancels the caller's context while the session is open, releases, and
// then uses the top page. If the session's lifetime were tied to the caller's
// context, the page target would be gone and the last evaluate would hang to the
// test deadline instead of answering.
func TestCrossOriginFrameSessionTeardownLeavesTheTabAlone(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	handles, err := frameHandles(ctx)
	if err != nil || len(handles) != 1 {
		t.Fatalf("frame handles: %v (%d)", err, len(handles))
	}
	callerCtx, callerCancel := context.WithCancel(ctx)
	workCtx, release, err := attachFrameTarget(callerCtx, handles[0].targetID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	// The caller's operation gives up. Its cancellation must stop the WORK…
	callerCancel()
	deadline := time.Now().Add(5 * time.Second)
	for workCtx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if workCtx.Err() == nil {
		t.Fatal("cancelling the caller did not stop work in the frame session")
	}
	// …and must not, by itself, tear the session down.
	time.Sleep(400 * time.Millisecond)
	release()

	var title string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.title`, &title)); err != nil {
		t.Fatalf("the embedding tab did not survive the frame session teardown: %v", err)
	}
	if title != "frame host" {
		t.Fatalf("top document title = %q, want the fixture's page", title)
	}
}

// TestCrossOriginFrameRefsSurviveAReSnapshot extends the ref-stability contract
// (ref_stability_test.go) across the frame boundary. Refs inside an
// out-of-process iframe are minted by the same walker and stamped into that
// frame's own DOM, so they have to survive a re-snapshot, an SPA-style re-render
// that replaces the nodes, and the frame index staying put.
func TestCrossOriginFrameRefsSurviveAReSnapshot(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	first := frameSnapshotFor(t, ctx)
	goRef := refForName(first[0].Snapshot.Elements, "Frame Go")
	titleRef := refForName(first[0].Snapshot.Elements, "frame-title")
	if goRef == "" || titleRef == "" {
		t.Fatalf("frame controls missing on the first read: %v", frameNamesOf(first[0].Snapshot.Elements))
	}

	second := frameSnapshotFor(t, ctx)
	if got := refForName(second[0].Snapshot.Elements, "Frame Go"); got != goRef {
		t.Fatalf("button ref renumbered across a re-snapshot: was %q, now %q", goRef, got)
	}
	if got := refForName(second[0].Snapshot.Elements, "frame-title"); got != titleRef {
		t.Fatalf("input ref renumbered across a re-snapshot: was %q, now %q", titleRef, got)
	}
	if second[0].Index != first[0].Index {
		t.Fatalf("frame index moved between reads: %d then %d", first[0].Index, second[0].Index)
	}

	// Re-render inside the frame: brand new nodes (so the stamped attribute is
	// gone) with changed visible text. The stable identity is the name attribute,
	// exactly as in the top-document case.
	frames, err := frameHandles(ctx)
	if err != nil || len(frames) != 1 {
		t.Fatalf("frame handles: %v (%d)", err, len(frames))
	}
	frameCtx, release, err := attachFrameTarget(ctx, frames[0].targetID)
	if err != nil {
		t.Fatalf("attach for re-render: %v", err)
	}
	rerender := `(function(){
		var b = document.getElementById('go');
		b.outerHTML = '<button id="go" style="position:absolute;left:10px;top:20px;width:180px;height:40px">Go Now</button>';
		return true;
	})()`
	err = chromedp.Run(frameCtx, chromedp.Evaluate(rerender, nil))
	release()
	if err != nil {
		t.Fatalf("re-render inside the frame: %v", err)
	}

	third := frameSnapshotFor(t, ctx)
	if got := refForName(third[0].Snapshot.Elements, "Go Now"); got != goRef {
		t.Fatalf("button ref renumbered across an in-frame re-render: was %q, now %q", goRef, got)
	}
}

// TestCrossOriginFrameUsesTheSameWalkerAsATopLevelPage is the single-source-of-
// truth proof for Q5YB9D. The same document is read twice — once as an
// out-of-process iframe and once as an ordinary top-level page — and the two
// element lists must agree on refs, roles and names.
//
// A second extractor for frames (which is what the extension used to carry, and
// what a hand-rolled OOPIF reader would be) cannot pass this: it would have to
// reproduce the selector set, the role mapping, the usefulness filter, the
// ranking and the stable-key ref rules exactly, and stay in step with them
// forever.
func TestCrossOriginFrameUsesTheSameWalkerAsATopLevelPage(t *testing.T) {
	ctx, innerURL := crossOriginFixture(t, innerFrameDoc)

	frames := frameSnapshotFor(t, ctx)
	inFrame := frames[0].Snapshot.Elements
	if len(inFrame) == 0 {
		t.Fatalf("no elements read from the frame")
	}

	// Same document, loaded as the top-level page in a second tab.
	topCtx, topCancel := chromedp.NewContext(ctx)
	defer topCancel()
	if err := chromedp.Run(topCtx,
		chromedp.Navigate(innerURL),
		chromedp.WaitReady("body"),
	); err != nil {
		t.Fatalf("navigate the inner document as a page: %v", err)
	}
	asPage, err := EvaluateWithOptions(topCtx, SnapshotOptions{Mode: "all", IncludeBoxes: true})
	if err != nil {
		t.Fatalf("top-level snapshot of the inner document: %v", err)
	}

	if len(inFrame) != len(asPage.Elements) {
		t.Fatalf("frame read %d elements, the same document as a page read %d: %v vs %v",
			len(inFrame), len(asPage.Elements), frameNamesOf(inFrame), frameNamesOf(asPage.Elements))
	}
	for i := range inFrame {
		got, want := inFrame[i], asPage.Elements[i]
		if got.Ref != want.Ref || got.Role != want.Role || got.Name != want.Name || got.Tag != want.Tag {
			t.Fatalf("element %d differs between the frame read and the page read: %+v vs %+v", i, got, want)
		}
		if got.W <= 0 || got.H <= 0 {
			t.Fatalf("element %d came back from the frame without a box, so it cannot be placed in the page: %+v", i, got)
		}
	}
}

func frameRefsOf(elements []Element) []string {
	out := make([]string, 0, len(elements))
	for _, el := range elements {
		out = append(out, el.Ref)
	}
	return out
}

func frameNamesOf(elements []Element) []string {
	out := make([]string, 0, len(elements))
	for _, el := range elements {
		out = append(out, el.Role+":"+el.Name)
	}
	return out
}

// TestFrameReadGateAsksAboutTheOriginTheFrameIsServing is the other half of the
// frame-read gate: WHICH origin it is asked about.
//
// The walker derives a frame's origin from el.src. That is page-writable DOM —
// the same input the forged data-brw-xframe stamp comes from — and it names the
// origin the embedder ASKED for, not the one now answering: a frame that
// redirected after load, or a page that shadows the src property, serves a
// document that attribute never named. Checking consent against it turns a grant
// for a site the user trusts into a read of one they never saw, which is the same
// hole the redirect hop opened in the daemon-side fetch check. The target's own
// URL is the live answer, so that is what the gate is asked about.
func TestFrameReadGateAsksAboutTheOriginTheFrameIsServing(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)

	// The frame keeps serving localhost; only what the DOM says about it changes.
	lie := `(function(){
		var el = document.getElementById('embed');
		Object.defineProperty(el, 'src', { get: function(){ return 'https://granted.example/embed'; } });
		return el.src;
	})()`
	var reported string
	if err := chromedp.Run(ctx, chromedp.Evaluate(lie, &reported)); err != nil {
		t.Fatalf("shadow the frame's src: %v", err)
	}
	if reported != "https://granted.example/embed" {
		t.Fatalf("the fixture did not take the lie: el.src reads %q", reported)
	}

	var asked []string
	frames, err := SnapshotOutOfProcessFrames(ctx, SnapshotOptions{Mode: "all"}, func(origin string) error {
		asked = append(asked, origin)
		if origin == "https://granted.example" {
			return nil
		}
		return errors.New("no site permission grant for " + origin + " (scope read)")
	})
	if err != nil {
		t.Fatalf("snapshot out-of-process frames: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("the gate was asked %v, want exactly the one embedded frame", asked)
	}
	if !strings.Contains(asked[0], "localhost") {
		t.Fatalf("the gate was asked about %q, which is what the page CLAIMS the frame is; it is serving a localhost document", asked[0])
	}
	if len(frames) != 0 {
		t.Fatalf("a frame whose real origin nobody granted was read anyway: %d frames, first from %q", len(frames), frames[0].URL)
	}

	// Granting the origin it is really serving reads it, so the gate is what
	// decided rather than the lie having broken the walk.
	allowed, err := SnapshotOutOfProcessFrames(ctx, SnapshotOptions{Mode: "all"}, func(string) error { return nil })
	if err != nil || len(allowed) != 1 {
		t.Fatalf("an allowed frame was not read: %v (%d frames)", err, len(allowed))
	}
	if !strings.Contains(allowed[0].Origin, "localhost") {
		t.Fatalf("the frame is reported as origin %q, which is not the document it is serving", allowed[0].Origin)
	}
}
