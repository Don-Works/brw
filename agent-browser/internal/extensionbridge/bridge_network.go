package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
)

func filterBridgeCapturedRequests(requests []snapshot.CapturedRequest, filter string) []snapshot.CapturedRequest {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return requests
	}
	out := make([]snapshot.CapturedRequest, 0, len(requests))
	for _, r := range requests {
		if strings.Contains(strings.ToLower(r.URL), filter) {
			out = append(out, r)
		}
	}
	return out
}

// NetworkCapture installs the in-page interceptor (idempotent) over the bridge
// transport and drains the recorded requests. This works without the CDP Network
// domain because the interceptor is pure in-page JS — the required baseline for
// the extension bridge into real Chrome.
func (b *Bridge) NetworkCapture(ctx context.Context, filter string) ([]snapshot.CapturedRequest, error) {
	var installed json.RawMessage
	if err := b.evaluate(ctx, snapshot.NetworkCaptureInstallScript, "", &installed); err != nil {
		return nil, err
	}
	var requests []snapshot.CapturedRequest
	if err := b.evaluate(ctx, snapshot.NetworkCaptureDrainScript, "", &requests); err != nil {
		return nil, err
	}
	return filterBridgeCapturedRequests(requests, filter), nil
}

// ReplayRequest re-executes a request in-page via fetch over the bridge.
func (b *Bridge) ReplayRequest(ctx context.Context, params browser.ReplayRequestParams) (snapshot.ReplayResult, error) {
	opts := map[string]any{
		"method":  params.Method,
		"url":     params.URL,
		"headers": params.Headers,
		"body":    params.Body,
	}
	args, _ := json.Marshal(opts)
	expr := fmt.Sprintf("%s(%s)", snapshot.ReplayRequestScript, args)
	var result snapshot.ReplayResult
	if err := b.evaluate(ctx, expr, "", &result); err != nil {
		return snapshot.ReplayResult{}, err
	}
	return result, nil
}
