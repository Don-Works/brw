package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (b *Bridge) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	b.ensureWebMCP(ctx, b.cachedTabID(ctx))
	return b.snapshot(ctx, opts, false)
}

// snapshotLive re-walks the page without consulting the tab's cached snapshot,
// and refreshes that cache with what it reads. Used to observe an action's
// effect: the extension's cache-validity probe can only see DOM mutations, and a
// fill/select/checkbox writes a DOM PROPERTY (value, checked, selectedIndex)
// that mutates no node, so a cached read after such an action returns the
// pre-action page and the result claims the action changed nothing. Re-walking
// costs one in-page pass on an action that changed nothing visible; reporting an
// action as a no-op when it landed costs a duplicate write on the retry.
func (b *Bridge) snapshotLive(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return b.snapshot(ctx, opts, true)
}

func (b *Bridge) snapshot(ctx context.Context, opts snapshot.SnapshotOptions, skipCacheRead bool) (snapshot.PageSnapshot, error) {
	var snap snapshot.PageSnapshot
	opts.IncludeAX = false
	// A since-delta request must reach the in-page walker (which derives the delta
	// from live per-document state) and must bypass the outer snapshot cache in
	// BOTH directions: a cached full snapshot would defeat the delta, and caching a
	// delta-shaped (partial) result under the key would corrupt later full reads.
	// include_frames also bypasses the cache so the dynamic cross-origin-frame read
	// is never served stale.
	sinceDelta := opts.Since > 0
	bypassCache := sinceDelta || opts.IncludeFrames
	if !bypassCache && !skipCacheRead {
		if cached, ok := b.tryCachedSnapshot(ctx, opts); ok {
			return cached, nil
		}
	}
	// The walker installs once per document and every later snapshot ships only
	// the call. Before this, the bridge re-sent the whole walker source down the
	// websocket on EVERY snapshot of the same page — tens of kilobytes per call,
	// paid again for each find, each post-action observation and each settle
	// re-read. Direct CDP already worked this way; the expressions come from
	// snapshot.SnapshotCallExpressions so both transports run the same source and
	// therefore mint the same refs.
	hot, cold := snapshot.SnapshotCallExpressions(opts)
	if err := b.evaluateReadOnly(ctx, hot, "", &snap); err != nil || !snapshot.SnapshotLooksInstalled(snap) {
		snap = snapshot.PageSnapshot{}
		if coldErr := b.evaluateReadOnly(ctx, cold, "", &snap); coldErr != nil {
			return snap, coldErr
		}
	}
	if err := b.enforceFinalURL(ctx, snap.URL); err != nil {
		return snapshot.PageSnapshot{}, err
	}
	snap.Accessibility = snapshot.AccessibilitySummary{
		Available: false,
		Error:     "accessibility tree is unavailable through the Chrome extension bridge; use direct CDP attach for AX enrichment",
	}
	if opts.IncludeFrames {
		// Best-effort: read the controls INSIDE cross-origin iframes and merge them
		// with frame-qualified refs (f<i>:<ref>) + top-level click coords. It
		// succeeds only where the frame has a debugger sub-target the extension can
		// attach to and no other debugger already owns it.
		readBoxes := map[int]bool{}
		if frames, err := b.readCrossOriginFrames(ctx, opts); err == nil && len(frames) > 0 {
			_, readBoxes = snapshot.MergeCrossOriginFrames(&snap, frames)
		}
		// Every frame whose controls were NOT read still becomes a CLICKABLE
		// element (ref f<i>, cx/cy at its center), so include_frames never leaves a
		// visible frame unmentioned.
		snapshot.PromoteCrossOriginFrames(&snap, readBoxes)
	}
	if !bypassCache {
		b.storeCachedSnapshot(ctx, opts, snap)
	}
	return snap, nil
}

// readCrossOriginFrames asks the extension to read the interactive controls of
// each cross-origin (out-of-process) iframe in the active tab. The extension
// matches the tab's frame tree against the debugger target list, briefly attaches
// to each frame target to extract its controls, and detaches. Returns an empty
// slice (no error) when the extension predates the command so callers degrade to
// the same-origin-only snapshot.
func (b *Bridge) readCrossOriginFrames(ctx context.Context, opts snapshot.SnapshotOptions) ([]snapshot.CrossOriginFrame, error) {
	// Phase one asks WHICH third-party documents are embedded here. It runs no
	// expression: the daemon has to know the origins before it can decide which of
	// them the user has consented to brw reading, and the live frame URL is the
	// only place that answer is correct — an iframe that redirected after load is
	// serving an origin its src attribute never named.
	listed, err := b.callCrossOriginFrames(ctx, nil, "")
	if err != nil || len(listed) == 0 {
		return listed, err
	}
	allow := browser.FrameReadCheckFromContext(ctx)
	origins := make([]string, 0, len(listed))
	seen := map[string]bool{}
	for _, frame := range listed {
		if frame.Origin == "" || seen[frame.Origin] {
			continue
		}
		if allow != nil {
			if refused := allow(frame.Origin); refused != nil {
				continue
			}
		}
		seen[frame.Origin] = true
		origins = append(origins, frame.Origin)
	}
	if len(origins) == 0 {
		return listed, nil
	}
	// Phase two hands the extension the walker to run, and the exact origins it
	// may run it in. The extension used to hold a private selector list plus its
	// own role and name rules, which is a second implementation of the thing
	// ref_stability_test covers: two extractors that agree today and disagree
	// after the next change to either. include_boxes is what lets the daemon put
	// the frame's controls back into top-level coordinates without the extension
	// measuring anything.
	frameOpts := opts
	frameOpts.Since = 0
	frameOpts.IncludeFrames = false
	frameOpts.IncludeAX = false
	frameOpts.IncludeBoxes = true
	_, cold := snapshot.SnapshotCallExpressions(frameOpts)
	return b.callCrossOriginFrames(ctx, origins, cold)
}

// callCrossOriginFrames issues one read_cross_origin_frames message. A nil
// origins list is the enumerate-only form; a non-nil one names every origin the
// expression may run in, and the extension evaluates in no other.
func (b *Bridge) callCrossOriginFrames(ctx context.Context, origins []string, expression string) ([]snapshot.CrossOriginFrame, error) {
	params := map[string]any{"tabId": parseTabID(b.contextTabID(ctx))}
	if origins != nil {
		params["origins"] = origins
		params["expression"] = expression
	}
	raw, err := b.call(ctx, "read_cross_origin_frames", params)
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return nil, nil
		}
		return nil, err
	}
	var payload struct {
		Frames []snapshot.CrossOriginFrame `json:"frames"`
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return nil, fmt.Errorf("parse cross-origin frames: %w", jsonErr)
		}
	}
	return payload.Frames, nil
}

func (b *Bridge) tryCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, bool) {
	if opts.Mode == "all" || opts.IncludeHidden {
		return snapshot.PageSnapshot{}, false
	}
	tabID := b.contextTabID(ctx)
	raw, err := b.call(ctx, "cached_snapshot", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
	})
	if err != nil {
		return snapshot.PageSnapshot{}, false
	}
	var resp struct {
		Cached   bool                  `json:"cached"`
		Snapshot snapshot.PageSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || !resp.Cached {
		return snapshot.PageSnapshot{}, false
	}
	return resp.Snapshot, true
}

func (b *Bridge) storeCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions, snap snapshot.PageSnapshot) {
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return
	}
	_, _ = b.call(ctx, "snapshot_result", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
		"snapshot": snap,
	})
}

func snapshotCacheKey(opts snapshot.SnapshotOptions) string {
	opts.IncludeAX = false
	data, _ := json.Marshal(opts)
	return string(data)
}

func (b *Bridge) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	b.ensureWebMCP(ctx, b.cachedTabID(ctx))
	return b.find(ctx, opts, false)
}

// FindLive is Find without the tab's cached snapshot. It is what a
// locate-and-act resolves through (see browser.LiveFinder): the extension's
// cache-validity probe only sees DOM mutations, so after an earlier fill or
// select in the same batch a cached element list is the PRE-action page, and
// the decision to actuate, the exactly-one-match rule included, would be made
// from a page that no longer exists. Post-action observation already re-walks
// for the same reason.
func (b *Bridge) FindLive(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	return b.find(ctx, opts, true)
}

func (b *Bridge) find(ctx context.Context, opts snapshot.FindOptions, live bool) (snapshot.FindResult, error) {
	snapOpts := snapshot.SnapshotOptions{
		Query:         opts.Query,
		Text:          opts.Text,
		Role:          opts.Role,
		Limit:         opts.Limit,
		ViewportOnly:  opts.ViewportOnly,
		IncludeHidden: opts.IncludeHidden,
		// text_content is advertised by brw_find on every transport. Dropping it
		// here made the option a no-op on the extension bridge: the tool said it
		// would match visible prose and then matched only element metadata.
		TextContent: opts.TextContent,
	}
	snap, err := b.snapshot(ctx, snapOpts, live)
	if err != nil {
		return snapshot.FindResult{}, err
	}
	return snapshot.FindResult{
		URL:      snap.URL,
		Title:    snap.Title,
		Elements: snap.Elements,
		Metadata: snap.Metadata,
	}, nil
}

func (b *Bridge) Read(ctx context.Context) (readability.PageRead, error) {
	start := time.Now()
	var read readability.PageRead
	err := b.evaluateReadOnly(ctx, readability.ReadExpr(), "", &read)
	b.recordObservation(b.contextTabID(ctx), browser.TraceActionRead, read.URL, start, err)
	if err != nil {
		return readability.PageRead{}, err
	}
	return readability.Normalize(read), nil
}

func (b *Bridge) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	start := time.Now()
	var data snapshot.StructuredData
	err := b.evaluateReadOnly(ctx, snapshot.StructuredDataScript, "", &data)
	b.recordObservation(b.contextTabID(ctx), browser.TraceActionReadData, data.URL, start, err)
	return data, err
}
