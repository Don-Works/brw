package browser

import (
	"context"
	"fmt"
	"strings"

	"github.com/revitt/agent-browser/internal/snapshot"
)

// filterCapturedRequests applies a case-insensitive URL substring filter
// Go-side so both transports share identical filtering semantics.
func filterCapturedRequests(requests []snapshot.CapturedRequest, filter string) []snapshot.CapturedRequest {
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

// ReplayRequestParams is the shared request shape for guarded replay across both
// transports (direct-CDP Manager and extension Bridge).
type ReplayRequestParams struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// GuardReplayRequest enforces the non-negotiable purchase/payment safety rule:
// a replay whose URL (or method+URL) looks like checkout/payment/order placement
// is refused with an explicit error and never executed. It reuses the same
// PurchaseControlWarning heuristics that gate clicks, so the guard surface stays
// generic and consistent. Returns a non-nil error when the request MUST be
// blocked.
func GuardReplayRequest(method, url string) error {
	// URLs encode tokens with separators (place-order, place_order, /placeOrder,
	// checkout.do) rather than the spaced human labels PurchaseControlWarning was
	// written for. Normalise separators and camelCase into spaces so the SAME
	// generic phrase heuristics catch the path form too. The click-path callers
	// keep using PurchaseControlWarning directly, so this only widens the replay
	// guard, never the click behaviour.
	normalized := normalizeURLForGuard(url)
	if warning := PurchaseControlWarning(method+" "+normalized, normalized); warning != "" {
		return fmt.Errorf("replay blocked: %s (url=%q)", warning, url)
	}
	return nil
}

// normalizeURLForGuard rewrites a URL so token separators become spaces and
// camelCase boundaries are split, letting the spaced purchase-phrase heuristics
// match path-style controls. Purely lexical; no per-site knowledge.
func normalizeURLForGuard(url string) string {
	var b strings.Builder
	b.Grow(len(url) * 2)
	runes := []rune(url)
	for i, r := range runes {
		switch {
		case r == '-' || r == '_' || r == '/' || r == '.' || r == '?' || r == '&' || r == '=' || r == '+' || r == '%' || r == ':':
			b.WriteRune(' ')
		case r >= 'A' && r <= 'Z':
			// Split camelCase: insert a space before an uppercase run boundary.
			if i > 0 {
				prev := runes[i-1]
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
					b.WriteRune(' ')
				}
			}
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NetworkCapture installs the in-page interceptor (idempotent) and drains the
// recorded requests, optionally filtered by a case-insensitive URL substring.
// In-page is the required baseline so capture works on both transports.
func (m *Manager) NetworkCapture(ctx context.Context, filter string) ([]snapshot.CapturedRequest, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	// Arm the interceptor to re-install on every future document for this tab so
	// capture survives full navigations/reloads (direct-CDP transport). Done once
	// per tab; best-effort — a failure here must not break same-page capture.
	m.netCaptureMu.Lock()
	armed := m.netCaptureTabs[tabID]
	m.netCaptureMu.Unlock()
	if !armed {
		if regErr := snapshot.RegisterNetworkCaptureOnNewDocument(tabCtx); regErr == nil {
			m.netCaptureMu.Lock()
			if m.netCaptureTabs == nil {
				m.netCaptureTabs = map[string]bool{}
			}
			m.netCaptureTabs[tabID] = true
			m.netCaptureMu.Unlock()
		}
	}
	requests, err := snapshot.CaptureNetwork(tabCtx)
	if err != nil {
		return nil, err
	}
	return filterCapturedRequests(requests, filter), nil
}

// ReplayRequest re-executes a request in-page via fetch and returns the result.
// It BLOCKS purchase/payment/order-placement-looking requests before any network
// call is made.
func (m *Manager) ReplayRequest(ctx context.Context, params ReplayRequestParams) (snapshot.ReplayResult, error) {
	if err := GuardReplayRequest(params.Method, params.URL); err != nil {
		return snapshot.ReplayResult{}, err
	}
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.ReplayResult{}, err
	}
	defer cancel()
	return snapshot.ReplayRequest(tabCtx, params.Method, params.URL, params.Headers, params.Body)
}
