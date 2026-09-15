package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	cdpproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// frameDetachTimeout bounds the hand-rolled detach that replaces chromedp's
// context teardown. It runs on a context derived with WithoutCancel because the
// caller's context is usually already done by then.
const frameDetachTimeout = 2 * time.Second

// ErrCrossOriginFrameUnsupported is the NAMED capability error a backend returns
// when it is handed a ref that lives inside a cross-origin iframe and cannot
// attach a CDP session to that frame's own target. The alternative — quietly
// acting on the frame ELEMENT instead — reports success for a click that landed
// somewhere else entirely, which is the failure a ref exists to prevent.
var ErrCrossOriginFrameUnsupported = errors.New("ref names an element inside a cross-origin iframe, whose document this action cannot reach")

// ErrCrossOriginPointUnreachable is the NAMED error for a ref that DID resolve
// inside its frame but whose translated point is not a pixel belonging to that
// element: outside the frame's box, or outside the top-level viewport that CDP
// input clamps into. Dispatching there clicks the embedding document and reports
// success, which is the failure a ref exists to prevent.
var ErrCrossOriginPointUnreachable = errors.New("element inside a cross-origin iframe cannot be reached at a top-level point")

// CrossOriginRefError wraps the sentinel with the verb that was asked for and
// the transport's way out, so callers can both match on the capability
// (errors.Is) and read what to do instead.
func CrossOriginRefError(ref, action, remedy string) error {
	return fmt.Errorf("%w: %s cannot act on %q — %s", ErrCrossOriginFrameUnsupported, action, ref, remedy)
}

// frameRefPattern splits the two halves of a cross-origin frame ref: f<i> names
// the frame (by its index in the snapshot's cross_origin_frames metadata) and the
// optional remainder is a ref minted INSIDE that frame's document by the same
// walker that mints top-level refs, so it carries the disambiguation suffixes
// too (f0:e6_edit_2).
var frameRefPattern = regexp.MustCompile(`^f(\d+)(?::(.+))?$`)

// ParseFrameRef reports whether ref names a cross-origin frame (ok) and splits
// it into the frame index and the inner ref. inner is empty for a bare f<i>,
// which names the FRAME itself rather than anything inside it.
func ParseFrameRef(ref string) (index int, inner string, ok bool) {
	match := frameRefPattern.FindStringSubmatch(ref)
	if match == nil {
		return 0, "", false
	}
	i, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, "", false
	}
	return i, match[2], true
}

// IsCrossOriginElementRef reports whether ref names an element INSIDE a
// cross-origin iframe (f<i>:<inner>), as opposed to the frame box itself (f<i>).
// Every backend routes on this one predicate, so a ref shape cannot be handled
// by one action and silently mis-resolved by its sibling.
func IsCrossOriginElementRef(ref string) bool {
	_, inner, ok := ParseFrameRef(ref)
	return ok && inner != ""
}

// WaitConditionRef extracts the element ref out of a wait condition that names
// one ("ref:e12", "not_ref:e12"), and returns "" for every other condition.
//
// A wait carries its ref inside a string, so a guard that only looked at
// parameters called "ref" would walk straight past brw_wait — and a wait for a
// ref inside a cross-origin iframe can only ever time out, which reads as "the
// page never got there" rather than "brw cannot see that document".
func WaitConditionRef(condition string) string {
	for _, prefix := range []string{"ref:", "not_ref:"} {
		if strings.HasPrefix(condition, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(condition, prefix))
		}
	}
	return ""
}

// CrossOriginFrameBoxesScript re-walks the document and returns the top-level box
// of every cross-origin iframe, in the order the walker numbers them, stamping
// data-brw-xframe="<i>" on each frame element as it goes.
//
// The stamp and the box index come out of the SAME walk on purpose: the index is
// what an agent holds as f<i>, and the stamp is how brw finds that frame's node
// to learn which CDP target hosts it. Derived separately they could disagree, and
// then a ref would resolve against the wrong frame.
const CrossOriginFrameBoxesScript = `(function() {` + FrameWalkHelpers + `
  __abRootsCompute();
  return __abInaccessibleFrames;
})()`

// OutOfProcessFrame is one cross-origin iframe read through a CDP session
// attached to its own target: Snapshot is what the shared walker returned for
// that document, in that document's own coordinate space.
type OutOfProcessFrame struct {
	// Index is the frame's position in the snapshot's cross_origin_frames
	// metadata, so it is the same i that brw_frame and f<i> refs use.
	Index    int
	TargetID string
	URL      string
	Origin   string
	Box      frameBox
	Snapshot PageSnapshot
}

// frameHandle is one cross-origin iframe resolved to the CDP target that hosts
// its document.
type frameHandle struct {
	index    int
	targetID target.ID
	box      frameBox
	// liveURL is what the frame's own CDP target reports it is showing, which is
	// not always what the embedder's markup asked for.
	liveURL string
}

// origin is the origin consent has to be checked against: the one the frame is
// SERVING.
//
// The walker derives a frame's origin from el.src, which is page-writable DOM —
// the same input the forged data-brw-xframe stamp comes from. A page can shadow
// the src property, and a frame can simply redirect after load, so the attribute
// names the origin the embedder asked for rather than the one now answering. Both
// turn a grant for a site the user trusts into a read of a document they never
// saw. The target's own URL is the live answer and wins; the attribute is the
// fallback for a target reporting no usable URL (about:blank, srcdoc), whose
// document came from the embedder's own markup anyway.
func (h frameHandle) origin() string {
	if parsed, err := url.Parse(h.liveURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return h.box.origin
}

// crossOriginFrameBoxes walks the document for its cross-origin iframe boxes and
// stamps each frame element with its index.
func crossOriginFrameBoxes(ctx context.Context) ([]frameBox, error) {
	var raw []map[string]any
	if err := chromedp.Run(ctx, chromedp.Evaluate(CrossOriginFrameBoxesScript, &raw)); err != nil {
		return nil, err
	}
	boxes := make([]frameBox, 0, len(raw))
	for _, item := range raw {
		boxes = append(boxes, frameBox{
			x:      toFloat(item["x"]),
			y:      toFloat(item["y"]),
			w:      toFloat(item["width"]),
			h:      toFloat(item["height"]),
			origin: toString(item["origin"]),
		})
	}
	return boxes, nil
}

// outOfProcessTargets returns the set of iframe target ids the browser currently
// hosts. A cross-origin iframe that shares a PROCESS with its embedder (same
// site, different port — the common httptest shape) has no target of its own and
// is absent here, which is exactly how the caller tells "readable through a
// session" from "coordinate-only".
func outOfProcessTargets(ctx context.Context) (map[target.ID]*target.Info, error) {
	holder := chromedp.FromContext(ctx)
	if holder == nil || holder.Browser == nil {
		return nil, errors.New("no browser attached to this context")
	}
	infos, err := target.GetTargets().Do(cdpproto.WithExecutor(ctx, holder.Browser))
	if err != nil {
		return nil, err
	}
	out := make(map[target.ID]*target.Info, len(infos))
	for _, info := range infos {
		if info != nil && info.Type == "iframe" {
			out[info.TargetID] = info
		}
	}
	return out, nil
}

// frameHandles maps every cross-origin iframe of the current document to the CDP
// target hosting it. The mapping is exact rather than matched by URL: the walker
// stamps data-brw-xframe="<i>" on each frame element it could not enter, and
// DOM.describeNode reports that element's child frame id, which for an
// out-of-process iframe IS its target id.
func frameHandles(ctx context.Context) ([]frameHandle, error) {
	// Walk first: nothing carries data-brw-xframe until the walker has run, and on
	// a page brw has only navigated to that is the case for the very first read.
	boxes, err := crossOriginFrameBoxes(ctx)
	if err != nil {
		return nil, err
	}
	if len(boxes) == 0 {
		return nil, nil
	}
	targets, err := outOfProcessTargets(ctx)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	var nodes []*cdpproto.Node
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		root, err := dom.GetDocument().Do(ctx)
		if err != nil {
			return err
		}
		ids, err := dom.QuerySelectorAll(root.NodeID, "[data-brw-xframe]").Do(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			node, err := dom.DescribeNode().WithNodeID(id).Do(ctx)
			if err != nil {
				continue
			}
			nodes = append(nodes, node)
		}
		return nil
	})); err != nil {
		return nil, err
	}
	handles := make([]frameHandle, 0, len(nodes))
	seen := map[int]bool{}
	for _, node := range nodes {
		index, ok := xframeIndex(node)
		if !ok {
			continue
		}
		if index >= len(boxes) {
			continue
		}
		// data-brw-xframe lives in page-writable DOM, and the walk only re-stamps
		// frames it classified as inaccessible — so a forged stamp on another
		// frame-owner element survives it. Two nodes claiming the same index would
		// be resolved by DOM order, silently binding f<i> to a target whose
		// coordinates come from a different frame's box, so the walk is refused
		// instead of guessed at. An <object> or <embed> hosting a cross-origin
		// document has a frameId too, which is why the tag is checked as well.
		if !strings.EqualFold(node.NodeName, "IFRAME") {
			continue
		}
		if seen[index] {
			return nil, fmt.Errorf("more than one element on this page is stamped as cross-origin frame f%d, so an f%d ref cannot be bound to one frame; brw refuses to guess which", index, index)
		}
		seen[index] = true
		id := target.ID(node.FrameID.String())
		info, hosted := targets[id]
		if !hosted {
			// Cross-origin but in the embedder's process: no target to attach to, so
			// this frame stays coordinate-only.
			continue
		}
		live := ""
		if info != nil {
			live = info.URL
		}
		handles = append(handles, frameHandle{index: index, targetID: id, box: boxes[index], liveURL: live})
	}
	return handles, nil
}

// xframeIndex reads the data-brw-xframe index off a described node. CDP reports
// attributes as a flat [name, value, name, value…] list.
func xframeIndex(node *cdpproto.Node) (int, bool) {
	if node == nil {
		return 0, false
	}
	for i := 0; i+1 < len(node.Attributes); i += 2 {
		if node.Attributes[i] != "data-brw-xframe" {
			continue
		}
		index, err := strconv.Atoi(node.Attributes[i+1])
		if err != nil {
			return 0, false
		}
		return index, true
	}
	return 0, false
}

// attachFrameTarget opens a flat CDP session on one out-of-process iframe target
// and returns a context that evaluates in THAT document, plus a release func.
//
// Two things here are load-bearing, and both exist because chromedp's context
// teardown calls Target.closeTarget on whatever it attached to. For a page
// that is right. For a SUBFRAME host it closes the entire TAB — verified against
// headless Chrome: the page target disappears and every later command on it
// times out.
//
// So the session's lifetime is ours, not the caller's. The chromedp context is
// created from a parent that cannot be cancelled, so the teardown can only run
// from release, which clears the Target first and leaves it nothing to close.
// The caller's context still bounds the WORK, through a derived context that
// cancels with it — otherwise a tab operation hitting its deadline mid-read
// would tear down the session and take the tab with it.
func attachFrameTarget(ctx context.Context, id target.ID) (context.Context, func(), error) {
	holder := chromedp.FromContext(ctx)
	if holder == nil || holder.Browser == nil {
		return nil, nil, errors.New("no browser attached to this context")
	}
	browser := holder.Browser
	frameCtx, cancel := chromedp.NewContext(context.WithoutCancel(ctx), chromedp.WithTargetID(id))
	frameHolder := chromedp.FromContext(frameCtx)
	if err := chromedp.Run(frameCtx); err != nil {
		frameHolder.Target = nil
		cancel()
		return nil, nil, fmt.Errorf("attach to cross-origin frame target: %w", err)
	}
	sessionID := frameHolder.Target.SessionID
	workCtx, stopWork := context.WithCancel(frameCtx)
	stopPropagating := context.AfterFunc(ctx, stopWork)
	release := func() {
		stopPropagating()
		stopWork()
		frameHolder.Target = nil
		cancel()
		detachCtx, detachCancel := context.WithTimeout(context.WithoutCancel(ctx), frameDetachTimeout)
		defer detachCancel()
		_ = target.DetachFromTarget().WithSessionID(sessionID).Do(cdpproto.WithExecutor(detachCtx, browser))
	}
	return workCtx, release, nil
}

// SnapshotOutOfProcessFrames walks every cross-origin iframe of the current page
// that has a CDP target of its own, through a session attached to that target.
//
// It runs the SAME walker as the top document (EvaluateWithOptions), so roles,
// names, ranking, the frontier score and the stable-key ref rules are one
// implementation, not two — a second copy would drift and ref_stability_test
// would stop covering the frames an agent actually has to act in.
//
// allowOrigin, when non-nil, decides whether each frame's ORIGIN may be read at
// all. Reading a frame's document is a read of a third-party site, and the call
// that asked for it was authorized against the embedder; a frame this refuses is
// skipped here and surfaces as a clickable box instead.
func SnapshotOutOfProcessFrames(ctx context.Context, opts SnapshotOptions, allowOrigin func(origin string) error) ([]OutOfProcessFrame, error) {
	handles, err := frameHandles(ctx)
	if err != nil {
		return nil, err
	}
	frames := make([]OutOfProcessFrame, 0, len(handles))
	for _, handle := range handles {
		origin := handle.origin()
		if allowOrigin != nil {
			if err := allowOrigin(origin); err != nil {
				continue
			}
		}
		frameCtx, release, err := attachFrameTarget(ctx, handle.targetID)
		if err != nil {
			// One unreachable frame must not cost the caller the rest of the page.
			continue
		}
		frameOpts := opts
		// A delta is keyed to a document's own walker state; asking a frame for
		// "what changed since version N of the TOP document" would answer about a
		// version it never issued.
		frameOpts.Since = 0
		frameOpts.IncludeFrames = false
		frameOpts.IncludeBoxes = true
		snap, snapErr := EvaluateWithOptions(frameCtx, frameOpts)
		release()
		if snapErr != nil {
			continue
		}
		frames = append(frames, OutOfProcessFrame{
			Index:    handle.index,
			TargetID: handle.targetID.String(),
			URL:      snap.URL,
			Origin:   origin,
			Box:      handle.box,
			Snapshot: snap,
		})
	}
	return frames, nil
}

// MergeOutOfProcessFrames appends the controls read inside out-of-process iframes
// to snap.Elements with frame-qualified refs (f<i>:<inner>). The inner half is a
// real ref in that frame's document, so direct-CDP resolves it through a session
// attached to that frame's target: brw_click routes there. The other ref-taking
// verbs do not route yet and refuse such a ref by name (see
// GuardCrossOriginRefs), so cx/cy is carried too as their way through.
//
// It returns the number of elements appended and the set of frame indices that
// were read, which the caller feeds to PromoteCrossOriginFrames so a frame read
// by ref does not also appear as a coordinate box.
func MergeOutOfProcessFrames(snap *PageSnapshot, frames []OutOfProcessFrame) (int, map[int]bool) {
	read := map[int]bool{}
	if snap == nil || len(frames) == 0 {
		return 0, read
	}
	appended := 0
	for _, frame := range frames {
		if len(frame.Snapshot.Elements) == 0 {
			continue
		}
		for _, el := range frame.Snapshot.Elements {
			if el.Ref == "" {
				continue
			}
			el.Ref = fmt.Sprintf("f%d:%s", frame.Index, el.Ref)
			el.Source = []string{"frame", "dom"}
			el.Key = ""
			// cx/cy stay useful even though the ref resolves: they are the fallback
			// for every verb that does not route into the frame.
			if el.W > 0 && el.H > 0 {
				el.CX = frame.Box.x + el.X + el.W/2
				el.CY = frame.Box.y + el.Y + el.H/2
			}
			el.X, el.Y, el.W, el.H = 0, 0, 0, 0
			snap.Elements = append(snap.Elements, el)
			appended++
		}
		read[frame.Index] = true
	}
	if snap.Metadata == nil {
		snap.Metadata = map[string]any{}
	}
	snap.Metadata["cross_origin_frames_read"] = len(read)
	snap.Metadata["cross_origin_frame_elements"] = appended
	if appended > 0 {
		snap.Metadata["cross_origin_note"] = "Controls inside cross-origin iframes were read through a CDP session attached to each frame's own target and carry source:[\"frame\",\"dom\"] with refs f<i>:<ref>. Those refs RESOLVE: pass one to brw_click and the click lands in that frame. Other ref-taking verbs do not route into a cross-origin frame yet and say so by name."
	}
	return appended, read
}

// CrossOriginFrameRectScript re-measures frame <i>'s box in the TOP document,
// after optionally scrolling that frame into view or scrolling the page by a
// delta, and reports the viewport it was measured against.
//
// Nothing else moves the frame ELEMENT. ResolveBox scrolls its target into view
// inside the FRAME's own document, which shifts nothing in the embedder, so a
// frame below the fold translated to a point outside the top-level viewport —
// and CDP clamps a click there into the visible page, which put the click in the
// EMBEDDING document while the ref reported success.
const CrossOriginFrameRectScript = `(function(index, intoView, dx, dy) {
  var el = document.querySelector('[data-brw-xframe="' + String(index) + '"]');
  if (!el) return { ok: false };
  if (intoView) {
    try { el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' }); }
    catch (e) { try { el.scrollIntoView(); } catch (_) {} }
  } else if (dx || dy) {
    try { window.scrollBy(dx, dy); } catch (e) {}
  }
  var r = el.getBoundingClientRect();
  return {
    ok: r.width > 0 && r.height > 0,
    x: r.left,
    y: r.top,
    width: r.width,
    height: r.height,
    viewport_width: window.innerWidth,
    viewport_height: window.innerHeight
  };
})`

// CrossOriginFramePointScript asks the EMBEDDING document what it would hit at a
// point, and reports whether that is frame <i>.
//
// This is the property the arithmetic only approximates. A point can be inside
// the frame's box and inside the viewport and still belong to something else —
// a sticky header, a cookie banner, a modal scrim — and a click there enters the
// embedder while the ref that produced it names an element in the frame. The top
// document cannot see INTO the frame, so elementFromPoint over an out-of-process
// iframe returns the iframe element itself: hitting it is exactly the condition
// that says the event will be routed to that frame's renderer.
const CrossOriginFramePointScript = `(function(index, px, py) {
  var el = document.querySelector('[data-brw-xframe="' + String(index) + '"]');
  if (!el) return { ok: false, reason: 'the frame is no longer on the page' };
  if (px < 0 || py < 0 || px > window.innerWidth || py > window.innerHeight) {
    return { ok: false, reason: 'the point is outside the ' + Math.round(window.innerWidth) + 'x' + Math.round(window.innerHeight) + ' viewport' };
  }
  var hit = document.elementFromPoint(px, py);
  if (!hit) return { ok: false, reason: 'the embedding document paints nothing at that point' };
  if (hit === el) return { ok: true, reason: '' };
  var what = String(hit.tagName || '').toLowerCase();
  // The id is page-controlled and ends up in an error an operator reads, so it
  // is bounded rather than pasted whole.
  if (hit.id) what += '#' + String(hit.id).replace(/\s+/g, ' ').slice(0, 60);
  return { ok: false, reason: 'the embedding document hit-tests <' + what + '> there, not the frame' };
})`

// framePointCheck is what the embedder answered about one candidate point.
type framePointCheck struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// crossOriginPointAttempts bounds the measure/verify loop. A cross-origin
// scrollIntoView reaches the embedder through the browser process, so one pass
// can read a rect the frame is already leaving; more than a few passes means the
// page is moving under us and refusing is the honest answer.
const crossOriginPointAttempts = 4

// frameHitTest evaluates CrossOriginFramePointScript against the top document.
func frameHitTest(ctx context.Context, index int, px, py float64) (framePointCheck, error) {
	var check framePointCheck
	expr := fmt.Sprintf("%s(%d,%g,%g)", CrossOriginFramePointScript, index, px, py)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &check)); err != nil {
		return framePointCheck{}, err
	}
	return check, nil
}

// frameRect is one cross-origin iframe's live top-level box plus the viewport it
// was measured in.
type frameRect struct {
	OK             bool    `json:"ok"`
	X              float64 `json:"x"`
	Y              float64 `json:"y"`
	Width          float64 `json:"width"`
	Height         float64 `json:"height"`
	ViewportWidth  float64 `json:"viewport_width"`
	ViewportHeight float64 `json:"viewport_height"`
}

// crossOriginPointSlack absorbs the sub-pixel gap between the frame box the
// walker rounded and the rect measured here.
const crossOriginPointSlack = 2

func (r frameRect) containsPoint(px, py float64) bool {
	return px >= r.X-crossOriginPointSlack && px <= r.X+r.Width+crossOriginPointSlack &&
		py >= r.Y-crossOriginPointSlack && py <= r.Y+r.Height+crossOriginPointSlack
}

func (r frameRect) pointInViewport(px, py float64) bool {
	return px >= 0 && py >= 0 && px <= r.ViewportWidth && py <= r.ViewportHeight
}

// TopLevelScrollSettleScript resolves once the embedder has produced a frame.
//
// Input events for an out-of-process iframe are routed by the BROWSER process
// from compositor hit-test data, which is only updated when the embedder commits
// a frame. Dispatching a click in the same tick as the scroll that brought the
// frame into view therefore hit-tests against where the frame USED to be and
// delivers the event to the embedding document. The setTimeout is the floor: a
// backgrounded or throttled page can stop producing frames altogether, and a
// wait with no way out is worse than a click that reports what it did.
const TopLevelScrollSettleScript = `new Promise(function(resolve){
  var done = false;
  function finish(){ if (done) return; done = true; resolve(true); }
  try { setTimeout(finish, 150); } catch (e) { finish(); }
  try { requestAnimationFrame(function(){ requestAnimationFrame(finish); }); } catch (e) { finish(); }
})`

// settleTopLevelScroll waits for the embedder to commit the scroll just applied.
func settleTopLevelScroll(ctx context.Context) error {
	var ok bool
	return chromedp.Run(ctx, chromedp.Evaluate(TopLevelScrollSettleScript, &ok,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

// frameRectIn evaluates CrossOriginFrameRectScript against the top document.
func frameRectIn(ctx context.Context, index int, intoView bool, dx, dy float64) (frameRect, error) {
	var rect frameRect
	expr := fmt.Sprintf("%s(%d,%t,%g,%g)", CrossOriginFrameRectScript, index, intoView, dx, dy)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &rect)); err != nil {
		return frameRect{}, err
	}
	return rect, nil
}

// findFrameHandle locates the handle for frame index i on the current page.
func findFrameHandle(ctx context.Context, index int) (frameHandle, error) {
	handles, err := frameHandles(ctx)
	if err != nil {
		return frameHandle{}, err
	}
	for _, candidate := range handles {
		if candidate.index == index {
			return candidate, nil
		}
	}
	return frameHandle{}, fmt.Errorf("no out-of-process iframe f%d on this page; re-run brw_snapshot with include_frames to refresh frame refs", index)
}

// ResolveCrossOriginActionPoint resolves an f<i>:<inner> ref through a session
// attached to frame i's own target and returns the element's box translated into
// TOP-LEVEL viewport coordinates, which is the space CDP input speaks. It applies
// the actionability gate the ordinary click path applies, evaluated INSIDE the
// frame; a zero timeout skips that wait and resolves the box alone.
//
// Manager.Click refuses a hidden, disabled or overlaid element before it
// dispatches anything. Skipping that for a frame ref would make the one ref shape
// that actuates by coordinate also the one shape that clicks a disabled button
// and reports OK, so the same script runs in the frame's own context.
//
// allowOrigin carries the same decision SnapshotOutOfProcessFrames takes, for the
// same reason: this attaches to the frame's target too. It is the only exported
// way in, so there is no sibling entry point a future caller can reach the frame
// through while passing nil.
func ResolveCrossOriginActionPoint(ctx context.Context, ref string, actionableTimeoutMS int64, allowOrigin func(origin string) error) (ElementBox, error) {
	return resolveCrossOriginPoint(ctx, ref, actionableTimeoutMS, allowOrigin)
}

func resolveCrossOriginPoint(ctx context.Context, ref string, actionableTimeoutMS int64, allowOrigin func(origin string) error) (ElementBox, error) {
	index, inner, ok := ParseFrameRef(ref)
	if !ok || inner == "" {
		return ElementBox{}, fmt.Errorf("ref %q does not name an element inside a cross-origin iframe", ref)
	}
	handle, err := findFrameHandle(ctx, index)
	if err != nil {
		return ElementBox{}, err
	}
	// Reaching a ref inside the frame is a read of, and an action in, a THIRD
	// PARTY's document: this attaches a session to that frame's target, evaluates
	// brw's actionability and box scripts there, and scrolls the frame's element.
	// The call was authorized against the origin the TAB is showing, which is not
	// a grant to actuate what that origin embeds — and nothing here requires a
	// prior include_frames read, because findFrameHandle does its own walk, so
	// guessing f0:e1 blind would otherwise reach the payment form unasked.
	if allowOrigin != nil {
		origin := handle.origin()
		if err := allowOrigin(origin); err != nil {
			return ElementBox{}, fmt.Errorf("%q is inside the document %s is serving, and acting there reads and actuates that third party rather than the page this call was authorized against: %w", ref, origin, err)
		}
	}
	// Bring the frame ELEMENT into the embedder's viewport first: everything after
	// this measures against where the frame actually is on screen.
	rect, err := frameRectIn(ctx, index, true, 0, 0)
	if err != nil {
		return ElementBox{}, err
	}
	if !rect.OK {
		return ElementBox{}, fmt.Errorf("%w: %q names frame f%d, which has no visible box in the embedding document; re-run brw_snapshot with include_frames to refresh frame refs", ErrCrossOriginPointUnreachable, ref, index)
	}
	frameCtx, release, err := attachFrameTarget(ctx, handle.targetID)
	if err != nil {
		return ElementBox{}, err
	}
	defer release()
	if actionableTimeoutMS > 0 {
		actionable, waitErr := WaitForActionableResult(frameCtx, inner, actionableTimeoutMS)
		if waitErr != nil {
			return ElementBox{}, waitErr
		}
		if !actionable.OK {
			return ElementBox{}, fmt.Errorf("element ref %q is not actionable within %dms inside cross-origin frame f%d — it may be hidden, disabled, or covered by an overlay; re-run brw_snapshot with include_frames to refresh frame refs", ref, actionableTimeoutMS, index)
		}
	}
	box, err := ResolveBox(frameCtx, inner)
	if err != nil {
		return ElementBox{}, err
	}
	// The frame's document reports its own viewport; the embedder's box is what
	// puts those pixels back into the top-level space.
	box.Ref = ref
	localX, localY := box.X, box.Y
	localViewportX, localViewportY := box.ViewportX, box.ViewportY
	translate := func(r frameRect) {
		box.X = localX + r.X
		box.Y = localY + r.Y
		box.ViewportX = localViewportX + r.X
		box.ViewportY = localViewportY + r.Y
	}
	check := framePointCheck{Reason: "the frame never settled in the embedding document"}
	for attempt := 0; attempt < crossOriginPointAttempts; attempt++ {
		// ResolveBox scrolled the element into view inside the FRAME, and that
		// request propagates out to the embedder through the browser process. The
		// rect read straight after it is therefore one the frame is about to leave,
		// which is why the loop settles, measures and then VERIFIES rather than
		// trusting a single measurement.
		if err := settleTopLevelScroll(ctx); err != nil {
			return ElementBox{}, err
		}
		rect, err = frameRectIn(ctx, index, false, 0, 0)
		if err != nil {
			return ElementBox{}, err
		}
		if !rect.OK {
			return ElementBox{}, fmt.Errorf("%w: %q names frame f%d, which has no visible box in the embedding document; re-run brw_snapshot with include_frames to refresh frame refs", ErrCrossOriginPointUnreachable, ref, index)
		}
		translate(rect)
		if rect.containsPoint(box.ViewportX, box.ViewportY) && !rect.pointInViewport(box.ViewportX, box.ViewportY) {
			// The frame is in view but taller (or wider) than the viewport and the
			// element sits in the clipped part. The frame's own document cannot scroll
			// it any further — it is already inside the FRAME's viewport — so move the
			// PAGE, then go round again to measure where that left things.
			moved, moveErr := frameRectIn(ctx, index, false,
				box.ViewportX-rect.ViewportWidth/2, box.ViewportY-rect.ViewportHeight/2)
			if moveErr != nil {
				return ElementBox{}, moveErr
			}
			if moved.OK && moved != rect {
				continue
			}
		}
		check, err = frameHitTest(ctx, index, box.ViewportX, box.ViewportY)
		if err != nil {
			return ElementBox{}, err
		}
		if check.OK {
			return box, nil
		}
	}
	// The point is not the frame's pixel, so a click there lands in the embedding
	// document — the exact failure a ref exists to prevent. Refuse: an unreachable
	// element must not come back as a box something is about to click.
	return ElementBox{}, fmt.Errorf("%w: %q resolves to (%.0f,%.0f) in the embedding document, but %s; scroll the page, close whatever covers the frame, or act by coordinate after brw_screenshot",
		ErrCrossOriginPointUnreachable, ref, box.ViewportX, box.ViewportY, check.Reason)
}
