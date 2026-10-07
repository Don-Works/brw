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

const maxBlockedRequestRecords = 100

// BlockedRequest is one subresource or navigation the containment boundary refused.
type BlockedRequest struct {
	URL          string `json:"url"`
	ResourceType string `json:"resource_type"`
	Reason       string `json:"reason"`
	At           string `json:"at"`
}

type containmentState struct {
	mu sync.Mutex

	armed   map[string]bool
	enables map[string]*sync.Mutex
	blocked map[string][]BlockedRequest

	inlineDocument map[string][]string

	documentResponse map[string]documentResponse
}

type documentResponse struct {
	status    int
	challenge *AuthChallenge
}

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

func (c *containmentState) lastDocumentResponse(tabID string) (int, *AuthChallenge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := c.documentResponse[tabID]
	return response.status, response.challenge
}

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

func (m *Manager) armInlineDocument(ctx context.Context, tabID, destination string) func() {
	m.containment.resetDocumentResponse(tabID)
	pattern := inlineDocumentPattern(destination)
	if pattern == "" {
		return func() {}
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return func() {}
	}
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

func (m *Manager) answerResponseStage(runCtx context.Context, tabID string, paused *fetch.EventRequestPaused) error {
	if !m.containment.inlineDocumentArmed(tabID) || paused.ResourceType != network.ResourceTypeDocument ||
		paused.FrameID.String() != tabID {
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}

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
	if err != nil || len(body) > InlineDocumentBodyLimit {
		return fetch.ContinueResponse(paused.RequestID).Do(runCtx)
	}
	return fetch.FulfillRequest(paused.RequestID, paused.ResponseStatusCode).
		WithResponseHeaders(rewrite.Headers).
		WithBody(base64.StdEncoding.EncodeToString(body)).
		Do(runCtx)
}

func redirectLocation(paused *fetch.EventRequestPaused) string {
	if paused.ResponseStatusCode < 300 || paused.ResponseStatusCode > 399 {
		return ""
	}
	for _, header := range paused.ResponseHeaders {
		if header == nil || !strings.EqualFold(header.Name, "Location") {
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

func pausedAtResponse(paused *fetch.EventRequestPaused) bool {
	return paused.ResponseStatusCode != 0 || paused.ResponseErrorReason != "" || len(paused.ResponseHeaders) > 0
}

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

func (m *Manager) ensureContainment(tabID string, tabCtx context.Context) {

	if !m.navPolicy.Confines() && !m.contentNavGuard {
		return
	}
	m.armInterception(tabID, tabCtx)
}

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

		m.listenContentNavigation(tabID, ev)

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

	var guard string
	var guardErr error
	if confines {
		guard, guardErr = BuildContainmentGuard(m.navPolicy)
	}
	go func() {
		enableCtx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer cancel()

		_ = m.syncFetchInterception(enableCtx, tabID)
		if guardErr != nil || !confines {
			return
		}

		_ = chromedp.Run(enableCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(guard).Do(runCtx)
			return err
		}))

		_ = chromedp.Run(enableCtx, chromedp.Evaluate(guard, nil))
	}()
}

func (m *Manager) containmentVerdict(tabID string, paused *fetch.EventRequestPaused) (allow bool, reason string, errorReason network.ErrorReason) {

	if allowed, why := m.contentNavigationVerdict(tabID, paused); !allowed {
		return false, why, network.ErrorReasonAborted
	}

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
