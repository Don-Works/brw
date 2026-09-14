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
	mu sync.Mutex
	// armed records that the tab's single requestPaused/authRequired listener is
	// installed. It is never cleared while the tab lives: the listener is bound to
	// the tab context and a second one would answer each event twice. Whether
	// Chrome is currently PAUSING requests is a separate question, decided by
	// syncFetchInterception on every call that changes what the tab needs.
	armed   map[string]bool
	enables map[string]*sync.Mutex
	blocked map[string][]BlockedRequest
}

// forget drops a closed tab's containment bookkeeping. Safe only once the tab is
// gone: clearing armed for a live tab would let a second listener be installed
// and every paused request would then be answered twice.
func (c *containmentState) forget(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.armed, tabID)
	delete(c.enables, tabID)
	delete(c.blocked, tabID)
}

func (c *containmentState) initLocked() {
	if c.armed == nil {
		c.armed = make(map[string]bool)
	}
	if c.enables == nil {
		c.enables = make(map[string]*sync.Mutex)
	}
	if c.blocked == nil {
		c.blocked = make(map[string][]BlockedRequest)
	}
}

// enableLock serialises Fetch.enable and Fetch.disable for one tab. Both the
// flags an enable carries and the enable/disable choice itself are read from
// state that other calls mutate, so the read and the command it produces have to
// happen under one lock or two callers can land in an order that leaves the tab
// enabled without auth handling while a credential is armed.
func (c *containmentState) enableLock(tabID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	lock, ok := c.enables[tabID]
	if !ok {
		lock = &sync.Mutex{}
		c.enables[tabID] = lock
	}
	return lock
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
	// The content boundary rides the same interception, so it arms it too: with
	// no navigation policy configured there is otherwise nothing intercepting
	// document requests and the boundary would silently do nothing.
	if !m.navPolicy.Confines() && !m.contentNavGuard {
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
		// Where the tab actually landed, for the content boundary. Delivered on
		// this same subscription rather than a second one.
		m.listenContentNavigation(tabID, ev)
		// An auth challenge is answered from the credential armed for this tab, or
		// deferred to Chrome when none is. It shares this listener because CDP
		// delivers it on the same connection as requestPaused and a second
		// listener would be a second answer to the same event.
		if auth, isAuth := ev.(*fetch.EventAuthRequired); isAuth {
			response := m.env.authResponse(tabID, auth.Request.URL)
			go func() {
				answerCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
				defer cancel()
				_ = chromedp.Run(answerCtx, fetch.ContinueWithAuth(auth.RequestID, response))
			}()
			return
		}
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		allow, reason, errorReason := m.containmentVerdict(tabID, paused)
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
			route = m.routes.match(tabID, paused.Request.URL, paused.ResourceType)
		}
		go func() {
			answerCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
			defer cancel()
			if !allow {
				_ = chromedp.Run(answerCtx, fetch.FailRequest(paused.RequestID, errorReason))
				return
			}
			if route != nil {
				_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
					return m.answerRoute(runCtx, tabID, route, paused)
				}))
				return
			}
			_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
				return m.continueWithEnvironmentHeaders(runCtx, tabID, paused)
			}))
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
		// Routed through syncFetchInterception, which reads the tab's needs and
		// sends the command under one per-tab lock, so this late-landing enable
		// cannot clear the handleAuthRequests flag a credential armed meanwhile.
		_ = m.syncFetchInterception(enableCtx, tabID)
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
// The third return value is the network error Chrome should report for a
// refusal. It is not cosmetic: ERR_BLOCKED_BY_CLIENT on a top-level navigation
// makes Chrome commit an error page AT the refused URL, so refusing a
// content-initiated navigation that way still moved the agent — to a Chrome
// error page on the attacker's URL. ERR_ABORTED leaves the current document in
// place, which is what "this navigation does not happen" has to mean.
func (m *Manager) containmentVerdict(tabID string, paused *fetch.EventRequestPaused) (allow bool, reason string, errorReason network.ErrorReason) {
	// The content boundary is checked first and independently of the domain
	// policy: "this page tried to steer the agent" is refused whether or not the
	// destination would otherwise have been reachable.
	if allowed, why := m.contentNavigationVerdict(tabID, paused); !allowed {
		return false, why, network.ErrorReasonAborted
	}
	// Interception may be armed for routes alone, with no policy to enforce.
	if !m.navPolicy.Confines() {
		return true, "", ""
	}
	url := paused.Request.URL
	var err error
	if paused.ResourceType == network.ResourceTypeDocument {
		err = m.navPolicy.Check(url)
	} else {
		err = m.navPolicy.CheckSubresource(url)
	}
	if err != nil {
		return false, err.Error(), network.ErrorReasonBlockedByClient
	}
	return true, "", ""
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
