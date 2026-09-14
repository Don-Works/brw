package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
)

// TestContentInitiatedNavigationIsRefusedWhileTheAgentsIsAllowed drives real
// Chrome. The same destination is reached twice: once because the PAGE asked,
// once because the AGENT asked. Only the second must land.
func TestContentInitiatedNavigationIsRefusedWhileTheAgentsIsAllowed(t *testing.T) {
	var offSiteHits int64
	offSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&offSiteHits, 1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><p id="landed">off-site</p></body></html>`)
	}))
	defer offSite.Close()
	// httptest binds 127.0.0.1; addressing the same listener as "localhost"
	// makes it a different HOST, which is what the boundary compares.
	offSiteURL := asLocalhost(offSite.URL) + "/destination"

	cases := []struct {
		name string
		// body is the page the agent is sent to; each variant tries a different
		// way for page content to move the browser.
		body string
	}{
		{
			name: "script assignment to location",
			body: `<script>window.location.href = %q;</script>`,
		},
		{
			name: "injected link click",
			body: `<script>
				var a = document.createElement('a');
				a.href = %q; a.textContent = 'go';
				document.body.appendChild(a);
				a.click();
			</script>`,
		},
		{
			name: "meta refresh",
			body: `<meta http-equiv="refresh" content="0; url=%s">`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atomic.StoreInt64(&offSiteHits, 0)
			format := c.body
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<!doctype html><html><body><p id="here">start</p>`+format+`</body></html>`, offSiteURL)
			}))
			defer page.Close()

			m := newHeadlessManager(t)
			m.SetContentNavigationGuard(true)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			var id target.ID
			if err := m.runBrowser(ctx, func(rc context.Context) error {
				var e error
				id, e = target.CreateTarget("about:blank").Do(rc)
				return e
			}); err != nil {
				t.Fatalf("create target: %v", err)
			}
			tabID := string(id)
			m.refs.SetActive(tabID)
			// Touching the tab context is what arms interception.
			if _, err := m.tabContext(tabID); err != nil {
				t.Fatalf("tab context: %v", err)
			}

			if _, err := m.NavigateTo(ctx, page.URL); err != nil {
				t.Fatalf("agent navigation to the start page failed: %v", err)
			}
			// Give the page's own navigation attempt time to be made and refused.
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if len(m.contentNavBlocked(tabID)) > 0 {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}

			blocked := m.BlockedRequests(tabID)
			named := false
			for _, b := range blocked {
				if strings.Contains(b.Reason, "content-initiated navigation refused") {
					named = true
				}
			}
			if !named {
				t.Fatalf("the page's navigation was not refused by name; blocked=%+v", blocked)
			}
			if got := atomic.LoadInt64(&offSiteHits); got != 0 {
				t.Fatalf("the off-site destination was reached %d times by page-initiated navigation", got)
			}
			tab, err := m.tabByID(ctx, tabID)
			if err != nil {
				t.Fatalf("tab lookup: %v", err)
			}
			if strings.Contains(tab.URL, "/destination") {
				t.Fatalf("the agent was moved to %q by page content", tab.URL)
			}

			// The SAME navigation, requested by the agent, must succeed.
			if _, err := m.NavigateTo(ctx, offSiteURL); err != nil {
				t.Fatalf("agent-requested navigation to the same destination failed: %v", err)
			}
			if got := atomic.LoadInt64(&offSiteHits); got == 0 {
				t.Fatal("the agent's own navigation to the destination never reached it")
			}
			tab, err = m.tabByID(ctx, tabID)
			if err != nil {
				t.Fatalf("tab lookup: %v", err)
			}
			if !strings.Contains(tab.URL, "/destination") {
				t.Fatalf("after the agent navigated, the tab is on %q", tab.URL)
			}
		})
	}
}

// contentNavBlocked peeks at the refusals without draining them, so the polling
// loop above does not consume the record the assertions then read.
func (m *Manager) contentNavBlocked(tabID string) []BlockedRequest {
	m.containment.mu.Lock()
	defer m.containment.mu.Unlock()
	m.containment.initLocked()
	return append([]BlockedRequest(nil), m.containment.blocked[tabID]...)
}

// TestContentNavigationVerdict exercises the decision itself, including the
// cases a browser test cannot easily stage: subframes and redirect hops.
func TestContentNavigationVerdict(t *testing.T) {
	const tabID = "tab-1"
	cases := []struct {
		name      string
		guard     bool
		committed string
		mainFrame cdp.FrameID
		intent    string
		anyIntent bool
		intentAge time.Duration
		// intentTTL is the bound the recording site gave this intent. Zero means
		// the navigation one; an interaction records a much shorter bound.
		intentTTL time.Duration
		chain     network.RequestID
		paused    *fetch.EventRequestPaused
		allow     bool
	}{
		{
			name:      "guard off allows anything",
			guard:     false,
			committed: "https://start.test/",
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "cross-site document with no agent intent is refused",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     false,
		},
		{
			name:      "the destination the agent asked for is allowed",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			intent:    "https://elsewhere.test/x",
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "a stale agent intent no longer explains a navigation",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			intent:    "https://elsewhere.test/x",
			intentAge: agentIntentTTL + time.Second,
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     false,
		},
		{
			name:      "a history move has no named destination and is allowed",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			anyIntent: true,
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "the agent's own click explains the navigation it caused",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			anyIntent: true,
			intentTTL: agentInteractionTTL,
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "a page that moves long after the agent's click is not that click",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			anyIntent: true,
			intentTTL: agentInteractionTTL,
			intentAge: agentInteractionTTL + time.Second,
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     false,
		},
		{
			name:      "same-host navigation by the page is ordinary routing",
			guard:     true,
			committed: "https://start.test/a",
			mainFrame: "frame-1",
			paused:    documentRequest("https://start.test/b", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "a subdomain of the current site is the same site",
			guard:     true,
			committed: "https://start.test/a",
			mainFrame: "frame-1",
			paused:    documentRequest("https://app.start.test/b", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "a cross-site subframe document is not a navigation",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			paused:    documentRequest("https://elsewhere.test/x", "frame-2", "net-1"),
			allow:     true,
		},
		{
			name:      "a redirect hop of an agent navigation stays agent-initiated",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			chain:     "net-1",
			paused:    documentRequest("https://third.test/x", "frame-1", "net-1"),
			allow:     true,
		},
		{
			name:      "a subresource is left to the containment rules",
			guard:     true,
			committed: "https://start.test/",
			mainFrame: "frame-1",
			paused: &fetch.EventRequestPaused{
				Request:      &network.Request{URL: "https://elsewhere.test/img.png"},
				ResourceType: network.ResourceTypeImage,
				FrameID:      "frame-1",
			},
			allow: true,
		},
		{
			name:      "nothing committed yet means the first document is the agent's",
			guard:     true,
			mainFrame: "frame-1",
			paused:    documentRequest("https://elsewhere.test/x", "frame-1", "net-1"),
			allow:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Manager{contentNavGuard: c.guard}
			m.contentNav.mu.Lock()
			m.contentNav.initLocked()
			if c.committed != "" {
				m.contentNav.committed[tabID] = c.committed
			}
			if c.mainFrame != "" {
				m.contentNav.mainFrames[tabID] = c.mainFrame
			}
			if c.intent != "" || c.anyIntent {
				ttl := c.intentTTL
				if ttl == 0 {
					ttl = agentIntentTTL
				}
				m.contentNav.intents[tabID] = agentIntent{
					host: hostOfURL(c.intent),
					any:  c.anyIntent,
					at:   time.Now().Add(-c.intentAge),
					ttl:  ttl,
				}
			}
			if c.chain != "" {
				m.contentNav.chains[tabID] = []network.RequestID{c.chain}
			}
			m.contentNav.mu.Unlock()

			allow, reason := m.contentNavigationVerdict(tabID, c.paused)
			if allow != c.allow {
				t.Fatalf("verdict allow=%v (%s), want %v", allow, reason, c.allow)
			}
			if !allow && !strings.Contains(reason, "content-initiated navigation refused") {
				t.Fatalf("the refusal must name itself, got %q", reason)
			}
		})
	}
}

// TestFrameNavigatedRetiresTheAgentIntent proves an agent navigation explains
// exactly one arrival: once the tab has landed, a page cannot re-use the intent
// to move it again.
func TestFrameNavigatedRetiresTheAgentIntent(t *testing.T) {
	const tabID = "tab-1"
	m := &Manager{contentNavGuard: true}
	m.recordAgentNavigation(tabID, "https://elsewhere.test/x")
	if allow, _ := m.contentNavigationVerdict(tabID, documentRequest("https://elsewhere.test/x", "frame-1", "net-1")); !allow {
		t.Fatal("the agent's own navigation was refused")
	}
	m.noteFrameNavigated(tabID, &cdp.Frame{ID: "frame-1", URL: "https://elsewhere.test/x"})
	m.noteFrameNavigated(tabID, &cdp.Frame{ID: "frame-1", URL: "https://start.test/"})
	if allow, _ := m.contentNavigationVerdict(tabID, documentRequest("https://elsewhere.test/x", "frame-1", "net-2")); allow {
		t.Fatal("a spent agent intent still explained a page-initiated navigation")
	}
}

func TestSameSiteHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"example.test", "example.test", true},
		{"app.example.test", "example.test", true},
		{"example.test", "app.example.test", true},
		{"example.test", "elsewhere.test", false},
		{"notexample.test", "example.test", false},
		{"", "example.test", false},
		{"example.test", "", false},
	}
	for _, c := range cases {
		if got := sameSiteHost(c.a, c.b); got != c.want {
			t.Errorf("sameSiteHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func documentRequest(rawURL string, frame cdp.FrameID, networkID network.RequestID) *fetch.EventRequestPaused {
	return &fetch.EventRequestPaused{
		Request:      &network.Request{URL: rawURL},
		ResourceType: network.ResourceTypeDocument,
		FrameID:      frame,
		NetworkID:    networkID,
	}
}

// TestAgentClickOnACrossSiteLinkIsAllowed drives real Chrome. The agent clicks
// a link it chose from the page, and the navigation that click causes is the
// agent's - refusing it would break every cross-site hand-off (an OAuth sign-in,
// a checkout passing to a payment processor) the moment the guard is armed.
func TestAgentClickOnACrossSiteLinkIsAllowed(t *testing.T) {
	var offSiteHits int64
	offSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&offSiteHits, 1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><p id="landed">off-site</p></body></html>`)
	}))
	defer offSite.Close()
	// The same listener under a different host string, which is what the
	// boundary compares.
	offSiteURL := asLocalhost(offSite.URL) + "/destination"

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><body><p id="here">start</p><a href=%q>Continue to partner</a></body></html>`, offSiteURL)
	}))
	defer page.Close()

	m := newHeadlessManager(t)
	m.SetContentNavigationGuard(true)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	tabID := string(id)
	m.refs.SetActive(tabID)
	if _, err := m.tabContext(tabID); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, page.URL); err != nil {
		t.Fatalf("agent navigation to the start page failed: %v", err)
	}

	if _, err := m.ClickText(ctx, snapshot.ClickTextOptions{Text: "Continue to partner"}); err != nil {
		t.Fatalf("the agent's own click was refused: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt64(&offSiteHits) == 0 {
		time.Sleep(200 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&offSiteHits); got == 0 {
		t.Fatalf("the destination the agent clicked through to was never reached; blocked=%+v", m.contentNavBlocked(tabID))
	}
	for _, blocked := range m.contentNavBlocked(tabID) {
		if strings.Contains(blocked.Reason, "content-initiated navigation refused") {
			t.Fatalf("the agent's own click was recorded as content-initiated: %s", blocked.Reason)
		}
	}
	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		t.Fatalf("tab lookup: %v", err)
	}
	if !strings.Contains(tab.URL, "/destination") {
		t.Fatalf("after the agent clicked the link, the tab is on %q", tab.URL)
	}
}

// TestAgentInputRecordsAnIntent walks the input verbs the manager records for
// and proves each one flips the verdict, and that a verb which only looks at the
// page does not: a navigation the agent did not ask for stays refused.
func TestAgentInputRecordsAnIntent(t *testing.T) {
	const tabID = "tab-1"
	verbs := []struct {
		action string
		allow  bool
	}{
		{"click", true},
		{"click_text", true},
		{"click_xy", true},
		{"click_button", true},
		{"press", true},
		{"type", true},
		{"fill", true},
		{"select", true},
		{"commit", true},
		{"drag", true},
		{"evaluate", true},
		{"upload_file", true},
		{"key_down", true},
		{"key_up", true},
		{"mouse_down", true},
		{"mouse_up", true},
		{"hover", false},
		{"scroll", false},
		{"snapshot", false},
		{"read", false},
	}
	for _, verb := range verbs {
		t.Run(verb.action, func(t *testing.T) {
			m := &Manager{contentNavGuard: true}
			m.contentNav.mu.Lock()
			m.contentNav.initLocked()
			m.contentNav.committed[tabID] = "https://start.test/"
			m.contentNav.mainFrames[tabID] = "frame-1"
			m.contentNav.mu.Unlock()

			m.recordAgentInteraction(tabID, verb.action)
			allow, reason := m.contentNavigationVerdict(tabID, documentRequest("https://elsewhere.test/x", "frame-1", "net-1"))
			if allow != verb.allow {
				t.Fatalf("after %s the verdict was allow=%v (%s), want %v", verb.action, allow, reason, verb.allow)
			}
		})
	}
}

// TestAgentInputActionsCoverEveryInputStep keeps the two tables in step: a batch
// step verb that actuates input but is missing from agentInputActions records no
// intent, so the navigation it causes is refused as the page's.
func TestAgentInputActionsCoverEveryInputStep(t *testing.T) {
	// Which verbs actuate input is not decided twice. siteconsent.StepActions
	// already classifies every verb the runners implement, and is itself checked
	// against their switches, so a new verb arrives here classified or fails
	// there. A second hand-written list of the observing verbs was the hole: a
	// new input verb added to the runner AND to that list passed this test while
	// recording no intent.
	for _, action := range planAndBatchStepActions(t) {
		class, classified := siteconsent.StepActions[action]
		if !classified {
			t.Errorf("step %q is implemented but siteconsent.StepActions does not classify it, so nothing decides whether it actuates input", action)
			continue
		}
		input := agentInputActions[action]
		if class == siteconsent.StepAct && !input {
			t.Errorf("step %q actuates input but is not in agentInputActions, so the navigation it causes is refused as content-initiated", action)
		}
		if class != siteconsent.StepAct && input {
			t.Errorf("step %q only observes or steers the tab itself, but agentInputActions treats it as the agent driving the page; a page that navigates because the agent scrolled would then be allowed", action)
		}
	}
}
