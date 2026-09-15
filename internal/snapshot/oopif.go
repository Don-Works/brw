package snapshot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	cdpproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
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
	for _, node := range nodes {
		index, ok := xframeIndex(node)
		if !ok {
			continue
		}
		if index >= len(boxes) {
			continue
		}
		id := target.ID(node.FrameID.String())
		if _, hosted := targets[id]; !hosted {
			// Cross-origin but in the embedder's process: no target to attach to, so
			// this frame stays coordinate-only.
			continue
		}
		handles = append(handles, frameHandle{index: index, targetID: id, box: boxes[index]})
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
func SnapshotOutOfProcessFrames(ctx context.Context, opts SnapshotOptions) ([]OutOfProcessFrame, error) {
	handles, err := frameHandles(ctx)
	if err != nil {
		return nil, err
	}
	frames := make([]OutOfProcessFrame, 0, len(handles))
	for _, handle := range handles {
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
			Origin:   handle.box.origin,
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

// ResolveCrossOriginBox resolves an f<i>:<inner> ref through a session attached
// to frame i's own target and returns the element's box translated into
// TOP-LEVEL viewport coordinates, which is the space CDP input speaks.
func ResolveCrossOriginBox(ctx context.Context, ref string) (ElementBox, error) {
	index, inner, ok := ParseFrameRef(ref)
	if !ok || inner == "" {
		return ElementBox{}, fmt.Errorf("ref %q does not name an element inside a cross-origin iframe", ref)
	}
	handles, err := frameHandles(ctx)
	if err != nil {
		return ElementBox{}, err
	}
	var handle frameHandle
	found := false
	for _, candidate := range handles {
		if candidate.index == index {
			handle = candidate
			found = true
			break
		}
	}
	if !found {
		return ElementBox{}, fmt.Errorf("no out-of-process iframe f%d on this page; re-run brw_snapshot with include_frames to refresh frame refs", index)
	}
	frameCtx, release, err := attachFrameTarget(ctx, handle.targetID)
	if err != nil {
		return ElementBox{}, err
	}
	defer release()
	box, err := ResolveBox(frameCtx, inner)
	if err != nil {
		return ElementBox{}, err
	}
	// The frame's document reports its own viewport; the embedder's box is what
	// puts those pixels back into the top-level space.
	box.Ref = ref
	box.X += handle.box.x
	box.Y += handle.box.y
	box.ViewportX += handle.box.x
	box.ViewportY += handle.box.y
	return box, nil
}
