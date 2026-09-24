package browser

import (
	"context"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
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
	// inlineDocument holds, per tab, the Fetch URL patterns under which the next
	// main-document response is rewritten so a text download renders as a page:
	// the destination's origin, plus the origin of each redirect it takes. Set by
	// one brw-driven navigation and cleared once its main document is answered.
	inlineDocument map[string][]string
	// documentResponse holds, per tab, the status and auth challenge of the main
	// document the last inline-document arm paused. Chrome under CDP cancels an
	// HTTP auth prompt and commits its error page, so the paused response is the
	// only place the challenge is visible.
	documentResponse map[string]documentResponse
}

type documentResponse struct {
	status    int
	challenge *AuthChallenge
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
	delete(c.inlineDocument, tabID)
	delete(c.documentResponse, tabID)
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
	if c.inlineDocument == nil {
		c.inlineDocument = make(map[string][]string)
	}
	if c.documentResponse == nil {
		c.documentResponse = make(map[string]documentResponse)
	}
}

func (c *containmentState) resetDocumentResponse(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.documentResponse, tabID)
}

func (c *containmentState) recordDocumentResponse(tabID string, paused *fetch.EventRequestPaused) {
	status := int(paused.ResponseStatusCode)
	response := documentResponse{status: status}
	if status == 401 || status == 407 {
		name := "www-authenticate"
		if status == 407 {
			name = "proxy-authenticate"
		}
		var values []string
		for _, header := range paused.ResponseHeaders {
			if header != nil && strings.EqualFold(header.Name, name) {
				values = append(values, header.Value)
			}
		}
		response.challenge = ParseAuthChallenge(values)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.documentResponse[tabID] = response
}

// lastDocumentResponse is the status and challenge recordDocumentResponse kept
// for the tab's last armed navigation; zero when none was paused.
func (c *containmentState) lastDocumentResponse(tabID string) (int, *AuthChallenge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := c.documentResponse[tabID]
	return response.status, response.challenge
}

// addInlineDocumentPattern arms the tab for pattern and reports whether it was
// not already armed for it.
func (c *containmentState) addInlineDocumentPattern(tabID, pattern string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	if slices.Contains(c.inlineDocument[tabID], pattern) {
		return false
	}
	c.inlineDocument[tabID] = append(c.inlineDocument[tabID], pattern)
	return true
}

func (c *containmentState) clearInlineDocument(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inlineDocument, tabID)
}

func (c *containmentState) inlineDocumentArmed(tabID string) bool {
	return len(c.inlineDocumentPatterns(tabID)) > 0
}

func (c *containmentState) inlineDocumentPatterns(tabID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.inlineDocument[tabID])
}

// inlineDocumentPattern is the Fetch URL pattern covering every document on
// rawURL's origin, or "" when rawURL is not an http(s) URL.
//
// The pattern is the origin rather than "*" so that a cross-site iframe never
// pauses under it. A frame document paused as Fetch.disable lands is neither
// reported nor released by Chrome, and the frame stays on about:blank for good;
// iframes load while the navigation that armed this is still settling.
func inlineDocumentPattern(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(u.Scheme == "http" && port == "80") && !(u.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return fetchPatternEscaper.Replace(u.Scheme+"://"+host) + "/*"
}

var fetchPatternEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`)

// armInlineDocument makes the tab's next main-document response from
// destination's origin render inline (see InlineDocumentHeaders) and returns the
// call that puts interception back the way it was. Both ends go through
// syncFetchInterception, so a tab that intercepts for containment keeps doing so
// and one that intercepted for this alone stops paying for it once the
// navigation has settled.
func (m *Manager) armInlineDocument(ctx context.Context, tabID, destination string) func() {
	pattern := inlineDocumentPattern(destination)
	if pattern == "" {
		return func() {}
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return func() {}
	}
	m.containment.resetDocumentResponse(tabID)
	m.containment.addInlineDocumentPattern(tabID, pattern)
	m.armInterception(tabID, tabCtx)
	_ = m.syncFetchInterception(ctx, tabID)
	return func() {
		m.containment.clearInlineDocument(tabID)
		disarmCtx, cancel := context.WithTimeout(tabCtx, m.timeout)
		defer cancel()
		_ = m.syncFetchInterception(disarmCtx, tabID)
	}
}

// answerResponseStage answers a request paused at the response stage. Only an
// inline-document arm asks Chrome to pause there, and only the main frame's
// document is rewritten; anything else continues untouched.
//
// Once the main frame's document is answered the arm has done its job and is
// released at once, not when the brw call returns. Fetch.disable landing while
// a frame's document is paused can strand that frame for good, and at this
// point the page has not yet parsed far enough to request any frame.
func (m *Manager) answerResponseStage(runCtx context.Context, tabID string, paused *fetch.EventRequestPaused) error {
	if !m.containment.inlineDocumentArmed(tabID) || paused.ResourceType != network.ResourceTypeDocument ||
		paused.FrameID.String() != tabID {
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}
	// A redirect off the armed origin would otherwise land its document outside
	// every pattern and download it. Arm the next origin before letting the
	// redirect proceed.
	if location := redirectLocation(paused); location != "" {
		if pattern := inlineDocumentPattern(location); pattern != "" && m.containment.addInlineDocumentPattern(tabID, pattern) {
			_ = m.syncFetchInterception(runCtx, tabID)
		}
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}
	m.containment.recordDocumentResponse(tabID, paused)
	err := answerInlineDocument(runCtx, paused)
	m.containment.clearInlineDocument(tabID)
	_ = m.syncFetchInterception(runCtx, tabID)
	return err
}

// answerInlineDocument answers the main frame's paused document response,
// rewriting it to render inline when it is a download-shaped text document.
func answerInlineDocument(runCtx context.Context, paused *fetch.EventRequestPaused) error {
	rewrite := InlineDocumentHeaders(paused.ResponseHeaders)
	switch {
	case !rewrite.Changed:
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	case !rewrite.NeedsBody:
		return fetch.ContinueResponse(paused.RequestID).
			WithResponseCode(paused.ResponseStatusCode).
			WithResponseHeaders(rewrite.Headers).
			Do(runCtx)
	case !InlineDocumentBodyWithinLimit(paused.ResponseHeaders):
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}
	body, err := fetch.GetResponseBody(paused.RequestID).Do(runCtx)
	if err != nil {
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}
	return fetch.FulfillRequest(paused.RequestID, paused.ResponseStatusCode).
		WithResponseHeaders(rewrite.Headers).
		WithBody(base64.StdEncoding.EncodeToString(body)).
		Do(runCtx)
}

// redirectLocation resolves the Location of a paused 3xx response against the
// request URL, or returns "" when the response is not a redirect.
func redirectLocation(paused *fetch.EventRequestPaused) string {
	if paused.ResponseStatusCode < 300 || paused.ResponseStatusCode > 399 {
		return ""
	}
	for _, header := range paused.ResponseHeaders {
		if !strings.EqualFold(header.Name, "Location") {
			continue
		}
		base, err := url.Parse(paused.Request.URL)
		if err != nil {
			return ""
		}
		next, err := base.Parse(strings.TrimSpace(header.Value))
		if err != nil {
			return ""
		}
		return next.String()
	}
	return ""
}

// pausedAtResponse reports whether a Fetch.requestPaused event carries a
// response, which is how CDP tells the two interception stages apart.
func pausedAtResponse(paused *fetch.EventRequestPaused) bool {
	return paused.ResponseStatusCode != 0 || paused.ResponseErrorReason != "" || len(paused.ResponseHeaders) > 0
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
		if pausedAtResponse(paused) {
			go func() {
				answerCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
				defer cancel()
				_ = chromedp.Run(answerCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
					return m.answerResponseStage(runCtx, tabID, paused)
				}))
			}()
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
