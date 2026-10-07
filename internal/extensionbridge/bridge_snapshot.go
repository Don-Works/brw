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

func (b *Bridge) snapshotLive(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return b.snapshot(ctx, opts, true)
}

func (b *Bridge) snapshot(ctx context.Context, opts snapshot.SnapshotOptions, skipCacheRead bool) (snapshot.PageSnapshot, error) {
	var snap snapshot.PageSnapshot
	opts.IncludeAX = false

	sinceDelta := opts.Since > 0
	bypassCache := sinceDelta || opts.IncludeFrames
	if !bypassCache && !skipCacheRead {
		if cached, ok := b.tryCachedSnapshot(ctx, opts); ok {
			if err := b.enforceFinalURL(ctx, cached.URL); err != nil {
				return snapshot.PageSnapshot{}, err
			}
			return cached, nil
		}
	}
	ctx, transport := withPageTransportNote(ctx)

	hot, cold := snapshot.SnapshotCallExpressions(opts)
	if err := b.evaluateReadOnly(ctx, hot, "", &snap); err != nil || !snapshot.SnapshotLooksInstalled(snap) {
		snap = snapshot.PageSnapshot{}
		if coldErr := b.evaluateReadOnly(ctx, cold, "", &snap); coldErr != nil {
			return snapshot.PageSnapshot{}, coldErr
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
		readBoxes := map[int]bool{}
		if frames, err := b.readCrossOriginFrames(ctx, opts); err == nil && len(frames) > 0 {
			_, readBoxes = snapshot.MergeCrossOriginFrames(&snap, frames)
		}

		snapshot.PromoteCrossOriginFrames(&snap, readBoxes)
		if err := b.guardCurrentURL(ctx); err != nil {
			return snapshot.PageSnapshot{}, err
		}
	}
	snap.Metadata = transport.apply(snap.Metadata)
	if !bypassCache {
		b.storeCachedSnapshot(ctx, opts, snap)
	}
	return snap, nil
}

func (b *Bridge) readCrossOriginFrames(ctx context.Context, opts snapshot.SnapshotOptions) ([]snapshot.CrossOriginFrame, error) {
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

	frameOpts := opts
	frameOpts.Since = 0
	frameOpts.IncludeFrames = false
	frameOpts.IncludeAX = false
	frameOpts.IncludeBoxes = true
	_, cold := snapshot.SnapshotCallExpressions(frameOpts)
	return b.callCrossOriginFrames(ctx, origins, cold)
}

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
		Frames                 []snapshot.CrossOriginFrame `json:"frames"`
		SkippedExtensionFrames int                         `json:"skippedExtensionFrames"`
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return nil, fmt.Errorf("parse cross-origin frames: %w", jsonErr)
		}
	}
	if check := browser.FrameReadCheckFromContext(ctx); check != nil {
		for i := range payload.Frames {
			frame := &payload.Frames[i]
			if frame.Snapshot == nil && len(frame.Elements) == 0 {
				continue
			}
			rawURL := frame.URL
			if frame.Snapshot != nil {
				rawURL = frame.Snapshot.URL
			}
			if rawURL == "" || check(rawURL) != nil {
				frame.Snapshot = nil
				frame.Elements = nil
			}
		}
	}
	pageTransportNoteFrom(ctx).noteSkipped(payload.SkippedExtensionFrames)
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

// FindLive is Find without the cached snapshot, used by locate-and-act (browser.LiveFinder): after a fill or select a cached list is the pre-action page, and the exactly-one-match decision would be made against it.
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
	err := b.evaluateReadOnly(ctx, readability.ReadExpr(readability.SettleMS(ctx)), "", &read)
	if err == nil {
		err = b.enforceFinalURL(ctx, read.URL)
	}
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
	if err == nil {
		err = b.enforceFinalURL(ctx, data.URL)
	}
	b.recordObservation(b.contextTabID(ctx), browser.TraceActionReadData, data.URL, start, err)
	if err != nil {
		return snapshot.StructuredData{}, err
	}
	return data, err
}
