package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

// The dashboard's two halves are proven separately elsewhere: the HTTP edge
// against a fake in this package, the dispatch-to-renderer half against real
// Chrome in internal/browser. Neither says the two are wired to each other. This
// runs the whole path once — an HTTP request from a loopback peer, through the
// daemon's own handlers, into a real browser — because "the routes work" and
// "the pixels moved" are different claims.
const dashboardE2EFixture = `<!doctype html><html><head><title>Dashboard fixture</title></head><body style="margin:0">
<button id="go" style="position:fixed;left:0;top:0;width:240px;height:80px">Go</button>
<output id="count" style="position:fixed;left:0;top:160px">0</output>
<script>
window.__clicks = 0;
document.getElementById('go').addEventListener('click', function(){
  window.__clicks++;
  document.getElementById('count').textContent = String(window.__clicks);
});
</script>
</body></html>`

func TestDashboardInputReachesTheRealBrowserAndAgentActionsAreRefused(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	t.Setenv(dashboardEnvVar, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	manager, err := browser.New(ctx, browser.Config{
		UserDataDir: t.TempDir(),
		Timeout:     20 * time.Second,
		Headless:    true,
		ChromeArgs:  []string{"--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--hide-scrollbars"},
	})
	if err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	defer manager.Close()

	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, dashboardE2EFixture)
	}))
	defer pages.Close()
	if _, err := manager.Open(ctx, pages.URL); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	server := New("127.0.0.1:17310", manager)

	// Nothing reaches the renderer before someone asks for the hold.
	press := `{"token":%q,"event":{"kind":"mouse","type":"mousePressed","x":120,"y":40,"button":"left","buttons":1,"click_count":1}}`
	release := `{"token":%q,"event":{"kind":"mouse","type":"mouseReleased","x":120,"y":40,"button":"left","click_count":1}}`
	ungranted := dashboardPost(t, server.dashboardInput, "/dashboard/input", fmt.Sprintf(press, "not-a-token"))
	if ungranted.Code != http.StatusForbidden {
		t.Fatalf("input without a grant = %d, want 403 (%s)", ungranted.Code, ungranted.Body.String())
	}
	if got := dashboardClickCount(t, ctx, manager); got != "0" {
		t.Fatalf("the page saw %s clicks before anyone enabled takeover", got)
	}

	acquired := dashboardPost(t, server.dashboardTakeover, "/dashboard/takeover", `{"action":"acquire"}`)
	if acquired.Code != http.StatusOK {
		t.Fatalf("acquire = %d (%s)", acquired.Code, acquired.Body.String())
	}
	var grant browser.TakeoverGrant
	if err := json.Unmarshal(acquired.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Token == "" || grant.TabID == "" {
		t.Fatalf("grant = %+v, want a token and the tab it is bound to", grant)
	}

	for _, body := range []string{fmt.Sprintf(press, grant.Token), fmt.Sprintf(release, grant.Token)} {
		if recorder := dashboardPost(t, server.dashboardInput, "/dashboard/input", body); recorder.Code != http.StatusOK {
			t.Fatalf("forward input = %d (%s)", recorder.Code, recorder.Body.String())
		}
	}
	if got := dashboardClickCount(t, ctx, manager); got != "1" {
		t.Fatalf("the page saw %s clicks after one forwarded click, want 1", got)
	}

	// And while that hold is live, the agent-facing route for the same browser
	// answers a conflict rather than driving the page the human is on.
	agent := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/page/click", strings.NewReader(`{"ref":"e1"}`))
	request.RemoteAddr = "127.0.0.1:54321"
	server.click(agent, request)
	if agent.Code != http.StatusConflict {
		t.Fatalf("agent click during a hold = %d, want 409 (%s)", agent.Code, agent.Body.String())
	}
	if !strings.Contains(agent.Body.String(), browser.TakeoverRefusedCode) {
		t.Errorf("agent refusal carries no %q code: %s", browser.TakeoverRefusedCode, agent.Body.String())
	}
	if got := dashboardClickCount(t, ctx, manager); got != "1" {
		t.Fatalf("the page saw %s clicks; a refused agent click reached the renderer", got)
	}

	released := dashboardPost(t, server.dashboardTakeover, "/dashboard/takeover",
		fmt.Sprintf(`{"action":"release","token":%q}`, grant.Token))
	if released.Code != http.StatusOK {
		t.Fatalf("release = %d (%s)", released.Code, released.Body.String())
	}
	if state := manager.TakeoverState(); state.Held {
		t.Fatalf("the hold survived its release: %+v", state)
	}
}

func dashboardPost(t *testing.T, handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:54321"
	handler(recorder, request)
	return recorder
}

// dashboardClickCount reads the counter the way brw_get reads it, which is the
// one evaluation path a hold leaves open.
func dashboardClickCount(t *testing.T, ctx context.Context, manager *browser.Manager) string {
	t.Helper()
	result, err := manager.Evaluate(browser.WithTraceLabel(ctx, browser.TraceActionGet, "text #count"),
		snapshot.BuildGetExpression("text", "#count", ""))
	if err != nil {
		t.Fatalf("read the click counter: %v", err)
	}
	envelope, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("get returned %T (%v), want the script envelope", result, result)
	}
	value, ok := envelope["value"].(string)
	if !ok {
		t.Fatalf("counter value is %T (%v), want the element text", envelope["value"], envelope["value"])
	}
	return strings.TrimSpace(value)
}
