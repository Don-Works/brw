package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	cdpproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const frameDetachTimeout = 2 * time.Second

// ErrCrossOriginFrameUnsupported is the NAMED capability error a backend returns when it is handed a ref that lives inside a cross-origin iframe and cannot attach a CDP session to that frame's own target.
var ErrCrossOriginFrameUnsupported = errors.New("ref names an element inside a cross-origin iframe, whose document this action cannot reach")

// ErrCrossOriginPointUnreachable is the NAMED error for a ref that DID resolve inside its frame but whose translated point is not a pixel belonging to that element: outside the frame's box, or outside the top-level viewport that CDP input clamps into.
var ErrCrossOriginPointUnreachable = errors.New("element inside a cross-origin iframe cannot be reached at a top-level point")

// CrossOriginRefError wraps the sentinel with the verb that was asked for and the transport's way out, so callers can both match on the capability (errors.Is) and read what to do instead.
func CrossOriginRefError(ref, action, remedy string) error {
	return fmt.Errorf("%w: %s cannot act on %q — %s", ErrCrossOriginFrameUnsupported, action, ref, remedy)
}

var frameRefPattern = regexp.MustCompile(`^f(\d+)(?::(.+))?$`)

// ParseFrameRef reports whether ref names a cross-origin frame (ok) and splits it into the frame index and the inner ref.
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

// IsCrossOriginElementRef reports whether ref names an element INSIDE a cross-origin iframe (f<i>:<inner>), as opposed to the frame box itself (f<i>).
func IsCrossOriginElementRef(ref string) bool {
	_, inner, ok := ParseFrameRef(ref)
	return ok && inner != ""
}

// WaitConditionRef extracts the element ref out of a wait condition that names one ("ref:e12", "not_ref:e12"), and returns "" for every other condition.
func WaitConditionRef(condition string) string {
	for _, prefix := range []string{"ref:", "not_ref:"} {
		if strings.HasPrefix(condition, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(condition, prefix))
		}
	}
	return ""
}

// CrossOriginFrameBoxesScript re-walks the document and returns the top-level box of every cross-origin iframe, in the order the walker numbers them, stamping data-brw-xframe="<i>" on each frame element as it goes.
const CrossOriginFrameBoxesScript = `(function() {` + FrameWalkHelpers + `
  __abRootsCompute();
  return __abInaccessibleFrames;
})()`

// OutOfProcessFrame is one cross-origin iframe read through a CDP session attached to its own target: Snapshot is what the shared walker returned for that document, in that document's own coordinate space.
type OutOfProcessFrame struct {
	// Index is the frame's position in the snapshot's cross_origin_frames metadata, so it is the same i that brw_frame and f<i> refs use.
	Index    int
	TargetID string
	URL      string
	Origin   string
	Box      frameBox
	Snapshot PageSnapshot
}

type frameHandle struct {
	index    int
	targetID target.ID
	box      frameBox

	liveURL string
}

func (h frameHandle) origin() string {
	if parsed, err := url.Parse(h.liveURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return h.box.origin
}

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

func frameHandles(ctx context.Context) ([]frameHandle, error) {

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

func xframeIndex(node *cdpproto.Node) (int, bool) {
	if node == nil {
		return 0, false
	}
	for i := 0; i+1 < len(node.Attributes); i += 2 {
		if node.Attributes[i] != "data-brw-xframe" {
			continue
		}
		index, err := strconv.Atoi(node.Attributes[i+1])
		if err != nil || index < 0 {
			return 0, false
		}
		return index, true
	}
	return 0, false
}

func attachFrameTarget(ctx context.Context, id target.ID) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	holder := chromedp.FromContext(ctx)
	if holder == nil || holder.Browser == nil {
		return nil, nil, errors.New("no browser attached to this context")
	}
	browser := holder.Browser
	frameCtx, cancel := chromedp.NewContext(context.WithoutCancel(ctx), chromedp.WithTargetID(id))
	frameHolder := chromedp.FromContext(frameCtx)
	workCtx, stopWork := context.WithCancel(frameCtx)
	stopPropagating := context.AfterFunc(ctx, stopWork)
	release := func() {
		stopPropagating()
		stopWork()
		var sessionID target.SessionID
		if frameHolder.Target != nil {
			sessionID = frameHolder.Target.SessionID
		}
		frameHolder.Target = nil
		cancel()
		if sessionID == "" {
			return
		}
		detachCtx, detachCancel := context.WithTimeout(context.WithoutCancel(ctx), frameDetachTimeout)
		defer detachCancel()
		_ = target.DetachFromTarget().WithSessionID(sessionID).Do(cdpproto.WithExecutor(detachCtx, browser))
	}
	if err := chromedp.Run(workCtx); err != nil {
		release()
		return nil, nil, fmt.Errorf("attach to cross-origin frame target: %w", err)
	}
	return workCtx, release, nil
}

// SnapshotOutOfProcessFrames walks every cross-origin iframe of the current page that has a CDP target of its own, through a session attached to that target.
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

			continue
		}
		frameOpts := opts

		frameOpts.Since = 0
		frameOpts.IncludeFrames = false
		frameOpts.IncludeBoxes = true
		snap, snapErr := EvaluateWithOptions(frameCtx, frameOpts)
		release()
		if snapErr != nil {
			continue
		}
		origin = (frameHandle{liveURL: snap.URL, box: handle.box}).origin()
		if allowOrigin != nil {
			if err := allowOrigin(origin); err != nil {
				continue
			}
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

// MergeOutOfProcessFrames appends the controls read inside out-of-process iframes to snap.Elements with frame-qualified refs (f<i>:<inner>).
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

// CrossOriginFrameRectScript re-measures frame <i>'s box in the TOP document, after optionally scrolling that frame into view or scrolling the page by a delta, and reports the viewport it was measured against.
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

// CrossOriginFramePointScript asks the EMBEDDING document what it would hit at a point, and reports whether that is frame <i>.
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

// CrossOriginFramePointerProbeScript asks the FRAME's own document which element the pointer is over, after a pointer move has been dispatched at the candidate point.
const CrossOriginFramePointerProbeScript = `(function(ref) {` + FrameWalkHelpers + `
  var hit = __abFindDeep(ref);
  if (!hit || !hit.el) return { ok: false, reason: 'the element is no longer in the frame' };
  // :hover matches an ancestor of the hovered node too, so a pointer over a span
  // inside the control still answers for the control.
  var hovered = false;
  try { hovered = hit.el.matches(':hover'); } catch (e) { hovered = false; }
  if (hovered) return { ok: true, reason: '' };
  var over = null;
  try { over = document.querySelectorAll(':hover'); } catch (e) { over = null; }
  var deepest = over && over.length ? over[over.length - 1] : null;
  if (!deepest) return { ok: false, reason: 'the pointer is not inside the frame at all' };
  var what = String(deepest.tagName || '').toLowerCase();
  // The id is page-controlled and ends up in an error an operator reads, so it
  // is bounded rather than pasted whole.
  if (deepest.id) what += '#' + String(deepest.id).replace(/\s+/g, ' ').slice(0, 60);
  return { ok: false, reason: 'the browser routes a pointer there to <' + what + '> inside the frame, not to the element' };
})`

type framePointCheck struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

const (
	crossOriginPointAttempts = 4
	crossOriginPointBudget   = 5 * time.Second
)

func frameHitTest(ctx context.Context, index int, px, py float64) (framePointCheck, error) {
	var check framePointCheck
	expr := fmt.Sprintf("%s(%d,%g,%g)", CrossOriginFramePointScript, index, px, py)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &check)); err != nil {
		return framePointCheck{}, err
	}
	return check, nil
}

func framePointerRouting(topCtx, frameCtx context.Context, ref string, px, py float64) (framePointCheck, error) {
	if err := chromedp.Run(topCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(input.MouseMoved, px, py).Do(ctx)
	})); err != nil {
		return framePointCheck{}, err
	}
	args, _ := json.Marshal(ref)
	var check framePointCheck
	expr := fmt.Sprintf("%s(%s)", CrossOriginFramePointerProbeScript, args)
	if err := chromedp.Run(frameCtx, chromedp.Evaluate(expr, &check)); err != nil {
		return framePointCheck{}, err
	}
	return check, nil
}

type frameRect struct {
	OK             bool    `json:"ok"`
	X              float64 `json:"x"`
	Y              float64 `json:"y"`
	Width          float64 `json:"width"`
	Height         float64 `json:"height"`
	ViewportWidth  float64 `json:"viewport_width"`
	ViewportHeight float64 `json:"viewport_height"`
}

const crossOriginPointSlack = 2

func (r frameRect) containsPoint(px, py float64) bool {
	return px >= r.X-crossOriginPointSlack && px <= r.X+r.Width+crossOriginPointSlack &&
		py >= r.Y-crossOriginPointSlack && py <= r.Y+r.Height+crossOriginPointSlack
}

func (r frameRect) pointInViewport(px, py float64) bool {
	return px >= 0 && py >= 0 && px <= r.ViewportWidth && py <= r.ViewportHeight
}

// TopLevelScrollSettleScript resolves once the embedder has produced a frame.
const TopLevelScrollSettleScript = `new Promise(function(resolve){
  var done = false;
  function finish(){ if (done) return; done = true; resolve(true); }
  try { setTimeout(finish, 150); } catch (e) { finish(); }
  try { requestAnimationFrame(function(){ requestAnimationFrame(finish); }); } catch (e) { finish(); }
})`

func settleTopLevelScroll(ctx context.Context) error {
	var ok bool
	return chromedp.Run(ctx, chromedp.Evaluate(TopLevelScrollSettleScript, &ok,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

func frameRectIn(ctx context.Context, index int, intoView bool, dx, dy float64) (frameRect, error) {
	var rect frameRect
	expr := fmt.Sprintf("%s(%d,%t,%g,%g)", CrossOriginFrameRectScript, index, intoView, dx, dy)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &rect)); err != nil {
		return frameRect{}, err
	}
	return rect, nil
}

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

// ResolveCrossOriginActionPoint resolves an f<i>:<inner> ref through a session attached to frame i's own target and returns the element's box translated into TOP-LEVEL viewport coordinates, which is the space CDP input speaks.
func ResolveCrossOriginActionPoint(ctx context.Context, ref string, actionableTimeoutMS int64, allowOrigin func(origin string) error) (ElementBox, error) {
	return resolveCrossOriginPoint(ctx, ref, actionableTimeoutMS, allowOrigin)
}

func resolveCrossOriginPoint(ctx context.Context, ref string, actionableTimeoutMS int64, allowOrigin func(origin string) error) (result ElementBox, resultErr error) {
	index, inner, ok := ParseFrameRef(ref)
	if !ok || inner == "" {
		return ElementBox{}, fmt.Errorf("ref %q does not name an element inside a cross-origin iframe", ref)
	}
	handle, err := findFrameHandle(ctx, index)
	if err != nil {
		return ElementBox{}, err
	}

	if allowOrigin != nil {
		origin := handle.origin()
		if err := allowOrigin(origin); err != nil {
			return ElementBox{}, fmt.Errorf("%q is inside the document %s is serving, and acting there reads and actuates that third party rather than the page this call was authorized against: %w", ref, origin, err)
		}
	}

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
	if allowOrigin != nil {
		defer func() {
			var tree *page.FrameTree
			err := chromedp.Run(frameCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				tree, err = page.GetFrameTree().Do(ctx)
				return err
			}))
			if err == nil && tree != nil && tree.Frame != nil {
				err = allowOrigin((frameHandle{liveURL: tree.Frame.URL, box: handle.box}).origin())
			} else if err == nil {
				err = errors.New("cross-origin frame returned no document metadata")
			}
			if err != nil {
				result, resultErr = ElementBox{}, err
			}
		}()
	}
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

	waitingOnRouting := false
	deadline := time.Now().Add(crossOriginPointBudget)

	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	for attempt := 0; attempt < crossOriginPointAttempts || (waitingOnRouting && time.Now().Before(deadline)); attempt++ {

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

			routed, probeErr := framePointerRouting(ctx, frameCtx, inner, box.ViewportX, box.ViewportY)
			if probeErr != nil {
				return ElementBox{}, probeErr
			}
			if routed.OK {
				return box, nil
			}
			check = routed
			waitingOnRouting = true
		} else {
			waitingOnRouting = false
		}
	}

	return ElementBox{}, fmt.Errorf("%w: %q resolves to (%.0f,%.0f) in the embedding document, but %s; scroll the page, close whatever covers the frame, or act by coordinate after brw_screenshot",
		ErrCrossOriginPointUnreachable, ref, box.ViewportX, box.ViewportY, check.Reason)
}
