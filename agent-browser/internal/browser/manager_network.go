package browser

import (
	"context"
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
func (m *Manager) ReplayRequest(ctx context.Context, params ReplayRequestParams) (snapshot.ReplayResult, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.ReplayResult{}, err
	}
	defer cancel()
	return snapshot.ReplayRequest(tabCtx, params.Method, params.URL, params.Headers, params.Body)
}
