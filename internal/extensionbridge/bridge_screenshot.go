package extensionbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const (
	bridgeScreenshotMaxWidth       = 800
	bridgeScreenshotJPEGQuality    = 50
	bridgeScreenshotAnnotateMaxDim = 900
)

func (b *Bridge) Screenshot(ctx context.Context) (browser.Screenshot, error) {
	tabID := b.contextTabID(ctx)
	params := map[string]any{"format": "jpeg", "quality": bridgeScreenshotJPEGQuality, "optimizeForSpeed": true}
	if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
		scale := 1.0
		if vw > bridgeScreenshotMaxWidth {
			scale = bridgeScreenshotMaxWidth / vw
		}
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": vw, "height": vh, "scale": scale}
		params["fallbackViewport"] = map[string]any{"width": vw, "height": vh}
	}
	return b.captureScreenshot(ctx, tabID, params)
}

// CaptureArtifactScreenshot returns the decoded bytes only; artifact storage does not need the base64 copy.
func (b *Bridge) CaptureArtifactScreenshot(ctx context.Context, ref string) (browser.Screenshot, error) {
	var (
		shot browser.Screenshot
		err  error
	)
	if strings.TrimSpace(ref) == "" {
		shot, err = b.Screenshot(ctx)
	} else {
		shot, err = b.ScreenshotElement(ctx, ref)
	}
	shot.Base64 = ""
	return shot, err
}

// DocumentIdentity returns Chrome's main-frame documentId (via chrome.webNavigation, no debugger attach), which changes on every committed replacement but not on same-document SPA history updates.
func (b *Bridge) DocumentIdentity(ctx context.Context) (browser.DocumentIdentity, error) {
	payload, err := b.extensionDocumentIdentity(ctx)
	if err != nil {
		return browser.DocumentIdentity{}, err
	}
	if payload.Origin == "" || payload.Origin == "null" || len(payload.Origin) > 2048 {
		return browser.DocumentIdentity{}, errors.New("extension returned an invalid main-document identity")
	}
	return browser.DocumentIdentity{
		ID:     payload.WorkerInstance + "\x00" + strconv.FormatInt(payload.TabID, 10) + "\x00" + payload.DocumentID + "\x00" + strconv.FormatUint(payload.DocumentEpoch, 16),
		Origin: payload.Origin,
	}, nil
}

func (b *Bridge) extensionDocumentIdentity(ctx context.Context) (extensionDocumentIdentityPayload, error) {
	tabID := b.contextTabID(ctx)
	params := map[string]any{}
	if tabID != "" {
		params["tabId"] = parseTabID(tabID)
	}
	raw, err := b.call(ctx, "get_document_identity", params)
	if err != nil {
		return extensionDocumentIdentityPayload{}, err
	}
	var payload extensionDocumentIdentityPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return extensionDocumentIdentityPayload{}, errors.New("extension returned an invalid main-document identity")
	}
	payload.DocumentID = strings.TrimSpace(payload.DocumentID)
	payload.WorkerInstance = strings.TrimSpace(payload.WorkerInstance)
	payload.Origin = strings.TrimSpace(payload.Origin)
	if payload.TabID <= 0 || payload.DocumentID == "" || len(payload.DocumentID) > 256 ||
		payload.WorkerInstance == "" || len(payload.WorkerInstance) > 256 || len(payload.Origin) > 2048 {
		return extensionDocumentIdentityPayload{}, errors.New("extension returned an invalid main-document identity")
	}
	return payload, nil
}

// CapturePDF returns decoded PDF bytes for artifact.Service; callers above see only an opaque artifact handle.
func (b *Bridge) CapturePDF(ctx context.Context) ([]byte, error) {
	tabID := b.contextTabID(ctx)
	raw, err := b.cdp(ctx, tabID, "Page.printToPDF", map[string]any{"printBackground": true})
	if err != nil {
		return nil, err
	}
	if err := b.guardCurrentURL(browser.WithTabID(ctx, tabID)); err != nil {
		return nil, err
	}
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Data == "" {
		return nil, errors.New("Page.printToPDF returned no data")
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return nil, fmt.Errorf("decode Page.printToPDF response: %w", err)
	}
	return data, nil
}

func (b *Bridge) ScreenshotElement(ctx context.Context, ref string) (browser.Screenshot, error) {
	if err := browser.GuardCrossOriginRefs("screenshot element", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.Screenshot{}, err
	}
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		return browser.Screenshot{}, err
	}
	tabID := b.contextTabID(ctx)
	params := map[string]any{
		"format":           "png",
		"optimizeForSpeed": true,
		"clip": map[string]any{
			"x":      box.X,
			"y":      box.Y,
			"width":  box.Width,
			"height": box.Height,
			"scale":  1,
		},
	}
	if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
		params["fallbackViewport"] = map[string]any{"width": vw, "height": vh}
	}
	return b.captureScreenshot(ctx, tabID, params)
}

func (b *Bridge) viewportDimensions(ctx context.Context, tabID string) (float64, float64) {
	var dims []float64
	if err := b.evaluate(ctx, `[Math.round(window.innerWidth), Math.round(window.innerHeight)]`, tabID, &dims); err != nil {
		return 0, 0
	}
	if len(dims) != 2 || dims[0] <= 0 || dims[1] <= 0 {
		return 0, 0
	}
	return dims[0], dims[1]
}

// ScreenshotAnnotated captures the page under a Set-of-Marks overlay and returns the PNG plus a ref->box legend.
func (b *Bridge) ScreenshotAnnotated(ctx context.Context, aopts browser.AnnotatedScreenshotOptions) (browser.AnnotatedScreenshot, error) {
	if err := browser.GuardCrossOriginRefs("screenshot annotate", browser.BridgeCrossOriginRemedy, aopts.Ref); err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	mode := aopts.Mode
	if strings.TrimSpace(mode) == "" {
		mode = snapshot.DefaultSnapshotMode
	}
	opts := snapshot.NormalizeOptions(snapshot.SnapshotOptions{Mode: mode})
	snap, err := b.Snapshot(ctx, opts)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	tabID := b.contextTabID(ctx)

	clip, clipErr := b.resolveAnnotationClip(ctx, tabID, aopts)
	if clipErr != nil {
		return browser.AnnotatedScreenshot{}, clipErr
	}

	marks := make([]snapshot.AnnotationMark, 0, len(snap.Elements))
	meta := make(map[string]snapshot.Element, len(snap.Elements))
	for _, el := range snap.Elements {
		if !el.InViewport {
			continue
		}
		marks = append(marks, snapshot.AnnotationMark{Ref: el.Ref, Name: el.Name, Role: el.Role})
		meta[el.Ref] = el
	}

	injectExpr, err := snapshot.InjectAnnotationOverlayExpr(marks)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	var overlay snapshot.AnnotationOverlayResult
	err = b.evaluate(ctx, injectExpr, tabID, &overlay)
	defer func() {
		var discard json.RawMessage
		_ = b.evaluate(ctx, snapshot.RemoveAnnotationOverlayExpr(), tabID, &discard)
	}()
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	capParams := map[string]any{"format": "png", "optimizeForSpeed": true}
	if clip != nil {
		scale := 1.0
		longest := clip.Width
		if clip.Height > longest {
			longest = clip.Height
		}
		if longest > bridgeScreenshotAnnotateMaxDim {
			scale = bridgeScreenshotAnnotateMaxDim / longest
		}
		capParams["clip"] = map[string]any{"x": clip.X, "y": clip.Y, "width": clip.Width, "height": clip.Height, "scale": scale}
	} else if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
		scale := 1.0
		longest := vw
		if vh > longest {
			longest = vh
		}
		if longest > bridgeScreenshotAnnotateMaxDim {
			scale = bridgeScreenshotAnnotateMaxDim / longest
		}
		capParams["clip"] = map[string]any{"x": 0, "y": 0, "width": vw, "height": vh, "scale": scale}
		capParams["fallbackViewport"] = map[string]any{"width": vw, "height": vh}
	}
	if _, ok := capParams["fallbackViewport"]; !ok {
		if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
			capParams["fallbackViewport"] = map[string]any{"width": vw, "height": vh}
		}
	}
	shot, err := b.captureScreenshot(ctx, tabID, capParams)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	legend := make(map[string]browser.LegendEntry, len(overlay.Legend))
	for _, box := range overlay.Legend {
		if !box.OK {
			continue
		}
		if clip != nil && !annotationBoxIntersects(box, clip) {
			continue
		}
		el := meta[box.Ref]
		legend[box.Ref] = browser.LegendEntry{
			Ref:    box.Ref,
			Name:   el.Name,
			Role:   el.Role,
			X:      box.X,
			Y:      box.Y,
			Width:  box.Width,
			Height: box.Height,
		}
	}

	return browser.AnnotatedScreenshot{
		MIMEType: shot.MIMEType,
		Data:     shot.Data,
		Base64:   shot.Base64,
		Legend:   legend,
	}, nil
}

func (b *Bridge) captureScreenshot(ctx context.Context, tabID string, params map[string]any) (browser.Screenshot, error) {
	raw, err := b.call(ctx, "capture_screenshot", map[string]any{
		"tabId":  parseTabID(tabID),
		"params": params,
	})
	if err != nil && isUnknownMessageTypeErr(err) {
		legacyParams := make(map[string]any, len(params))
		for key, value := range params {
			if key != "fallbackViewport" {
				legacyParams[key] = value
			}
		}
		raw, err = b.cdp(ctx, tabID, "Page.captureScreenshot", legacyParams)
	}
	if err != nil {
		return browser.Screenshot{}, err
	}
	if err := b.guardCurrentURL(browser.WithTabID(ctx, tabID)); err != nil {
		return browser.Screenshot{}, err
	}
	var payload struct {
		Data     string `json:"data"`
		Fallback string `json:"fallback"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return browser.Screenshot{}, err
	}
	if payload.Fallback != "pdf" {
		mimeType := "image/png"
		if format, _ := params["format"].(string); format == "jpeg" {
			mimeType = "image/jpeg"
		}
		return screenshotFromRawMIME(raw, mimeType)
	}
	pdf, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return browser.Screenshot{}, fmt.Errorf("decode screenshot PDF fallback: %w", err)
	}
	pngData, err := rasterizeBridgePDF(ctx, pdf, params)
	if err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{
		MIMEType: "image/png",
		Data:     pngData,
		Base64:   base64.StdEncoding.EncodeToString(pngData),
	}, nil
}

const annotationClipMargin = 18.0

type annotationClip struct {
	X, Y, Width, Height float64
}

func (b *Bridge) resolveAnnotationClip(ctx context.Context, tabID string, aopts browser.AnnotatedScreenshotOptions) (*annotationClip, error) {
	var x, y, w, h float64
	switch {
	case strings.TrimSpace(aopts.Ref) != "":
		box, err := b.resolveBox(browser.WithTabID(ctx, tabID), aopts.Ref)
		if err != nil {
			return nil, err
		}
		x = box.ViewportX - box.Width/2 - annotationClipMargin
		y = box.ViewportY - box.Height/2 - annotationClipMargin
		w = box.Width + 2*annotationClipMargin
		h = box.Height + 2*annotationClipMargin
	case !aopts.Region.IsZero():
		x = aopts.Region.X - annotationClipMargin
		y = aopts.Region.Y - annotationClipMargin
		w = aopts.Region.Width + 2*annotationClipMargin
		h = aopts.Region.Height + 2*annotationClipMargin
	default:
		return nil, nil
	}
	var dims struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	_ = b.evaluate(ctx, `({w: window.innerWidth||document.documentElement.clientWidth||0, h: window.innerHeight||document.documentElement.clientHeight||0})`, tabID, &dims)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if dims.W > 0 && x+w > dims.W {
		w = dims.W - x
	}
	if dims.H > 0 && y+h > dims.H {
		h = dims.H - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screenshot clip resolves to an empty region")
	}
	return &annotationClip{X: x, Y: y, Width: w, Height: h}, nil
}

func annotationBoxIntersects(box snapshot.AnnotationBox, clip *annotationClip) bool {
	return box.X < clip.X+clip.Width && box.X+box.Width > clip.X &&
		box.Y < clip.Y+clip.Height && box.Y+box.Height > clip.Y
}

func screenshotFromRawMIME(raw json.RawMessage, mimeType string) (browser.Screenshot, error) {
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return browser.Screenshot{}, err
	}
	if payload.Data == "" {
		return browser.Screenshot{}, errors.New("screenshot returned no data")
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: mimeType, Data: data, Base64: payload.Data}, nil
}
