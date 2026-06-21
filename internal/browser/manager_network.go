package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
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

// nonIdempotentReplayMethods can cause server-side state changes when replayed,
// so only these are subject to the transactional-URL guard. Idempotent reads
// (GET/HEAD) — the documented safe use of brw_replay_request — are never blocked.
var nonIdempotentReplayMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// transactionalURLMarkers flag a URL that looks like checkout / payment / order
// placement. Replaying a mutating request to one can move money or place an
// order, so it is refused before any network call.
var transactionalURLMarkers = []string{
	"checkout", "payment", "pay", "purchase",
	"place-order", "placeorder", "order", "charge", "billing", "subscribe",
}

// BlockedReplayReason returns a non-empty reason when a replay must be refused
// up front, never executed — implementing the safety contract advertised in the
// brw_replay_request tool schema. A mutating method (POST/PUT/PATCH/DELETE) to a
// transactional-looking URL is blocked; idempotent GET/HEAD reads are always
// allowed. Shared by both transports so the guarantee holds identically.
func (p ReplayRequestParams) BlockedReplayReason() string {
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method == "" {
		method = "GET"
	}
	if !nonIdempotentReplayMethods[method] {
		return ""
	}
	lower := strings.ToLower(p.URL)
	for _, marker := range transactionalURLMarkers {
		if strings.Contains(lower, marker) {
			return fmt.Sprintf("refusing to replay a %s request to a checkout/payment/order URL (matched %q); replay is limited to safe idempotent reads", method, marker)
		}
	}
	return ""
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
	if reason := params.BlockedReplayReason(); reason != "" {
		return snapshot.ReplayResult{}, errors.New(reason)
	}
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return snapshot.ReplayResult{}, err
	}
	defer cancel()
	return snapshot.ReplayRequest(tabCtx, params.Method, params.URL, params.Headers, params.Body)
}
