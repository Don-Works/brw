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
				m.contentNav.intents[tabID] = agentIntent{
					host: hostOfURL(c.intent),
					any:  c.anyIntent,
					at:   time.Now().Add(-c.intentAge),
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
