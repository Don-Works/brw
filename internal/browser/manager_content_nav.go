package browser

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
)

// agentIntentTTL bounds how long a recorded agent navigation stays eligible to
// explain an incoming document request. It exists so an intent that never
// committed (a dead host, an aborted load) does not sit there indefinitely
// explaining away a later content-initiated navigation to the same host.
const agentIntentTTL = 60 * time.Second

// maxAgentChains bounds the remembered redirect chains per tab.
const maxAgentChains = 64

// ErrContentInitiatedNavigation names the refusal the content boundary makes.
// It is a distinct sentinel because "blocked by policy" and "your page tried to
// steer the agent" are different facts, and an agent that cannot tell them apart
// retries the wrong one.
var ErrContentInitiatedNavigation = fmt.Errorf("content-initiated navigation refused")

// contentNavState distinguishes a navigation the AGENT asked for from one the
// PAGE asked for.
//
// The navigation policy answers "may this destination be reached". It does not
// answer "who asked", and without that a page can move the agent: an injected
// link click, a meta refresh or a script assignment to location produces a
// perfectly ordinary document request to a perfectly reachable host, and the
// agent's next snapshot is of a page it never asked to be on. On a signed-in
// profile that is the whole of a prompt-injection navigation attack.
//
// The signal is brw's own bookkeeping, not anything read out of the page: every
// entry point that steers the browser on the agent's behalf records its intent
// here first. A page cannot write to this, which is what makes it a boundary
// rather than a heuristic.
type contentNavState struct {
	mu sync.Mutex
	// intents holds the destination each tab's agent-requested navigation is
	// heading for. An empty host means "wherever history goes" (back/forward),
	// which the agent asked for without naming a destination.
	intents map[string]agentIntent
	// committed is the main-frame URL each tab last landed on.
	committed map[string]string
	// mainFrames maps a tab to its top-level frame id, so a subframe document
	// request is not mistaken for a top-level navigation.
	mainFrames map[string]cdp.FrameID
	// chains records the network request ids that belong to an agent-initiated
	// navigation, so its redirect hops stay agent-initiated.
	chains map[string][]network.RequestID
}

type agentIntent struct {
	host string
	any  bool
	at   time.Time
}

// SetContentNavigationGuard turns the content boundary on. Off by default: it
// changes which navigations succeed, and that is an operator decision.
func (m *Manager) SetContentNavigationGuard(on bool) { m.contentNavGuard = on }

// ContentNavigationGuard reports whether the boundary is armed.
func (m *Manager) ContentNavigationGuard() bool { return m.contentNavGuard }

func (c *contentNavState) initLocked() {
	if c.intents == nil {
		c.intents = map[string]agentIntent{}
	}
	if c.committed == nil {
		c.committed = map[string]string{}
	}
	if c.mainFrames == nil {
		c.mainFrames = map[string]cdp.FrameID{}
	}
	if c.chains == nil {
		c.chains = map[string][]network.RequestID{}
	}
}

// recordAgentNavigation notes that brw itself is about to steer tabID to rawURL.
// An empty rawURL records a history move, whose destination the agent did not
// name.
func (m *Manager) recordAgentNavigation(tabID, rawURL string) {
	if !m.contentNavGuard || tabID == "" {
		return
	}
	m.contentNav.mu.Lock()
	defer m.contentNav.mu.Unlock()
	m.contentNav.initLocked()
	if strings.TrimSpace(rawURL) == "" {
		m.contentNav.intents[tabID] = agentIntent{any: true, at: time.Now()}
		return
	}
	m.contentNav.intents[tabID] = agentIntent{host: hostOfURL(rawURL), at: time.Now()}
}

// noteFrameNavigated records where a tab actually landed and retires the intent
// that got it there.
func (m *Manager) noteFrameNavigated(tabID string, frame *cdp.Frame) {
	if !m.contentNavGuard || frame == nil || tabID == "" {
		return
	}
	if frame.ParentID != "" {
		return // a subframe, not the tab's own location
	}
	m.contentNav.mu.Lock()
	defer m.contentNav.mu.Unlock()
	m.contentNav.initLocked()
	m.contentNav.mainFrames[tabID] = frame.ID
	m.contentNav.committed[tabID] = frame.URL
	// The intent is spent once the tab has arrived. Leaving it would let a page
	// re-trigger the same navigation later and be waved through as the agent's.
	delete(m.contentNav.intents, tabID)
	delete(m.contentNav.chains, tabID)
}

// forgetContentNav drops a closed tab's bookkeeping.
func (c *contentNavState) forget(tabID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.intents, tabID)
	delete(c.committed, tabID)
	delete(c.mainFrames, tabID)
	delete(c.chains, tabID)
}

// contentNavigationVerdict decides one top-level document request.
//
// Returns allow=true for anything that is not a top-level document request, so
// subresources and subframes are left entirely to the containment rules.
func (m *Manager) contentNavigationVerdict(tabID string, paused *fetch.EventRequestPaused) (bool, string) {
	if !m.contentNavGuard || paused == nil {
		return true, ""
	}
	if paused.ResourceType != network.ResourceTypeDocument {
		return true, ""
	}
	m.contentNav.mu.Lock()
	defer m.contentNav.mu.Unlock()
	m.contentNav.initLocked()

	if main, known := m.contentNav.mainFrames[tabID]; known && paused.FrameID != "" && paused.FrameID != main {
		// A subframe document. An iframe loading a cross-origin page does not
		// move the agent, and refusing it would break ordinary pages.
		return true, ""
	}

	// A redirect hop of a navigation already accepted as agent-initiated stays
	// agent-initiated. Without this an allowed destination that redirects is
	// refused at the second hop, which reads as brw randomly failing on login
	// flows.
	for _, id := range m.contentNav.chains[tabID] {
		if id == paused.NetworkID && paused.NetworkID != "" {
			return true, ""
		}
	}

	destination := hostOfURL(paused.Request.URL)
	if destination == "" {
		// Not an http(s) destination: navigation policy already refuses the
		// dangerous non-network schemes, and there is no host boundary to draw.
		return true, ""
	}

	intent, hasIntent := m.contentNav.intents[tabID]
	if hasIntent && time.Since(intent.at) <= agentIntentTTL && (intent.any || intent.host == destination) {
		m.contentNav.rememberChainLocked(tabID, paused.NetworkID)
		return true, ""
	}

	committed := hostOfURL(m.contentNav.committed[tabID])
	if committed == "" {
		// Nothing has committed in this tab yet, so there is no page to have
		// initiated anything. A fresh tab's first document is the agent's.
		m.contentNav.rememberChainLocked(tabID, paused.NetworkID)
		return true, ""
	}
	if sameSiteHost(destination, committed) {
		// Ordinary in-app navigation. The boundary is cross-site movement, not
		// every link on the page the agent was sent to.
		return true, ""
	}
	return false, fmt.Sprintf("%s: %s tried to navigate the agent to %s, which brw did not request. Navigate deliberately with brw_navigate_to if that is where you meant to go",
		ErrContentInitiatedNavigation, committed, destination)
}

func (c *contentNavState) rememberChainLocked(tabID string, id network.RequestID) {
	if id == "" {
		return
	}
	chain := append(c.chains[tabID], id)
	if len(chain) > maxAgentChains {
		chain = chain[len(chain)-maxAgentChains:]
	}
	c.chains[tabID] = chain
}

// sameSiteHost reports whether two hosts belong to one site for the purposes of
// this boundary: identical, or one a subdomain of the other.
//
// It is an approximation and is documented as one. brw carries no public-suffix
// list, so this cannot distinguish "two subdomains of a registrable domain" from
// "two subdomains of a public suffix". The error is in the permissive direction
// for sibling subdomains, which is the behaviour ordinary multi-subdomain apps
// need; the cross-site case this exists to catch is unaffected.
func sameSiteHost(a, b string) bool {
	a = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a), "."))
	b = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b), "."))
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// hostOfURL is the host extraction this boundary compares on. Only http(s)
// destinations have a host boundary worth drawing; everything else yields "" and
// is left to the navigation policy, which already refuses the dangerous schemes.
func hostOfURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// listenContentNavigation wires the frame-navigated signal the boundary needs.
// Called from the same per-tab listener that serves containment so there is one
// subscription per tab, not two.
func (m *Manager) listenContentNavigation(tabID string, ev any) {
	if !m.contentNavGuard {
		return
	}
	if navigated, ok := ev.(*page.EventFrameNavigated); ok {
		m.noteFrameNavigated(tabID, navigated.Frame)
	}
}
