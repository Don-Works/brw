package browser

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// maxBlockedRequestRecords bounds the per-tab record of refused subresources.
const maxBlockedRequestRecords = 100

// BlockedRequest is one subresource or navigation the containment boundary
// refused.
type BlockedRequest struct {
	URL          string `json:"url"`
	ResourceType string `json:"resource_type"`
	Reason       string `json:"reason"`
	At           string `json:"at"`
}

type containmentState struct {
	mu      sync.Mutex
	armed   map[string]bool
	blocked map[string][]BlockedRequest
}

func (c *containmentState) initLocked() {
	if c.armed == nil {
		c.armed = make(map[string]bool)
	}
	if c.blocked == nil {
		c.blocked = make(map[string][]BlockedRequest)
	}
}

// ensureContainment installs subresource containment on a tab when a navigation
// policy is configured.
//
// The navigation policy alone gates only the URL an agent asks brw to open. An
// allowed page can still pull scripts, beacons, fetch/XHR and WebSockets from
// anywhere, so "the agent may only visit example.com" did not previously mean
// "this tab may only talk to example.com". Fetch interception is what makes the
// allowlist an actual confinement boundary rather than a navigation filter.
//
// Two limits are deliberate and documented rather than papered over:
//
//   - It is armed per tab, when brw first touches that tab. Requests issued
//     before that point are not seen.
//   - It contains network requests. It does not contain WebRTC, which reaches
//     the network without an interceptable request; a brw-launched browser gets
//     that closed at launch instead (see the --allowed-domains launch flags).
func (m *Manager) ensureContainment(tabID string, tabCtx context.Context) {
	if !m.navPolicy.Confines() {
		return
	}
	m.armInterception(tabID, tabCtx)
}

// armInterception enables CDP request interception on a tab exactly once, and
// installs the single handler that serves BOTH containment and routes.
//
// One handler is not an optimisation, it is a correctness requirement: CDP
// delivers Fetch.requestPaused once per request and it must be answered exactly
// once. Two independent listeners would both try to answer, and the second
// answer is a protocol error that leaves the request hanging.
//
// Order is deliberate. Containment is evaluated FIRST, so a route can never be
// used to reach a host the policy forbids; only then do routes get to answer.
func (m *Manager) armInterception(tabID string, tabCtx context.Context) {
	m.containment.mu.Lock()
	m.containment.initLocked()
	if m.containment.armed[tabID] {
		m.containment.mu.Unlock()
		return
	}
	m.containment.armed[tabID] = true
	m.containment.mu.Unlock()

	chromedp.ListenTarget(tabCtx, func(ev any) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		allow, reason := m.containmentVerdict(paused)
		if !allow {
			m.recordBlockedRequest(tabID, BlockedRequest{
				URL:          clipDialogText(paused.Request.URL),
				ResourceType: paused.ResourceType.String(),
				Reason:       reason,
				At:           time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
		var route *Route
		if allow {
			route = m.routes.match(tabID, paused.Request.URL)
		}
		go func() {
			answerCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
			defer cancel()
			if !allow {
				_ = chromedp.Run(answerCtx, fetch.FailRequest(paused.RequestID, network.ErrorReasonBlockedByClient))
				return
			}
			if route != nil {
				_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
					return applyRoute(runCtx, route, paused.RequestID)
				}))
				return
			}
			_ = chromedp.Run(answerCtx, fetch.ContinueRequest(paused.RequestID))
		}()
	})

	confines := m.navPolicy.Confines()
	// Only built when there is a policy to enforce; interception armed for
	// routes alone installs no in-page guard.
	var guard string
	var guardErr error
	if confines {
		guard, guardErr = BuildContainmentGuard(m.navPolicy)
	}
	go func() {
		enableCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer cancel()
		// One pattern matching everything: the allow/deny decision is ours, not
		// Chrome's, because an allowlist cannot be expressed as a URL blocklist.
		_ = chromedp.Run(enableCtx, fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}))
		if guardErr != nil || !confines {
			return
		}
		// Installed for every FUTURE document, before its own scripts run, which
		// is what lets the wrappers land before page code captures the originals.
		_ = chromedp.Run(enableCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(guard).Do(runCtx)
			return err
		}))
		// Also apply to the document already loaded in this tab. Page scripts
		// there may already hold pristine references, so this is best-effort
		// catch-up rather than a guarantee; Fetch interception still covers HTTP.
		_ = chromedp.Run(enableCtx, chromedp.Evaluate(guard, nil))
	}()
}

// containmentVerdict applies navigation rules to a document request and
// subresource rules to everything else.
//
// Gating the document request here is not redundant with the MCP-layer check: a
// permitted URL that redirects off the allowlist produces a document request
// that the MCP check never sees.
func (m *Manager) containmentVerdict(paused *fetch.EventRequestPaused) (allow bool, reason string) {
	// Interception may be armed for routes alone, with no policy to enforce.
	if !m.navPolicy.Confines() {
		return true, ""
	}
	url := paused.Request.URL
	var err error
	if paused.ResourceType == network.ResourceTypeDocument {
		err = m.navPolicy.Check(url)
	} else {
		err = m.navPolicy.CheckSubresource(url)
	}
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (m *Manager) recordBlockedRequest(tabID string, record BlockedRequest) {
	m.containment.mu.Lock()
	defer m.containment.mu.Unlock()
	m.containment.initLocked()
	entries := append(m.containment.blocked[tabID], record)
	if len(entries) > maxBlockedRequestRecords {
		entries = entries[len(entries)-maxBlockedRequestRecords:]
	}
	m.containment.blocked[tabID] = entries
}

// BlockedRequests returns (and clears) what containment refused for a tab.
// Silent blocking is how a contained page turns into an unexplained broken
// page, so the refusals are reportable.
func (m *Manager) BlockedRequests(tabID string) []BlockedRequest {
	m.containment.mu.Lock()
	defer m.containment.mu.Unlock()
	m.containment.initLocked()
	entries := m.containment.blocked[tabID]
	delete(m.containment.blocked, tabID)
	if entries == nil {
		return []BlockedRequest{}
	}
	return entries
}
