package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/siteconsent"
)

func standInRemoteManager(t *testing.T) *browser.Manager {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelLaunch()
	profile := browsertest.NewProfile(t)
	launcher, err := cdp.Launch(launchCtx, cdp.LaunchConfig{UserDataDir: profile.Dir(), Headless: true})
	if err != nil {
		t.Skipf("stand-in Chrome did not start: %v", err)
	}
	profile.StopWith(func() { _ = launcher.Close() })

	request, err := http.NewRequestWithContext(launchCtx, http.MethodGet, launcher.Endpoint()+"/json/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read the stand-in's CDP version document: %v", err)
	}
	defer response.Body.Close()
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		t.Fatalf("decode the stand-in's CDP version document: %v", err)
	}
	if version.WebSocketDebuggerURL == "" {
		t.Fatal("the stand-in published no websocket debugger URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager, err := browser.New(ctx, browser.Config{
		Timeout: 45 * time.Second,
		Remote: &browser.RemoteTarget{
			WebSocketURL: version.WebSocketDebuggerURL,
			RedactedURL:  "ws://stand-in.invalid",
			ProviderID:   "local.standin",
			SessionID:    "standin-mcp-1",
			ExpiresAt:    time.Now().Add(10 * time.Minute),
		},
	})
	if err != nil {
		t.Skipf("drive the stand-in over a remote target: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if !manager.Remote() {
		t.Fatal("the manager built over a RemoteTarget does not report itself as remote")
	}
	return manager
}

const remoteConsentFixture = `<!doctype html><html><head><meta charset="utf-8"><title>consent fixture</title></head>
<body>
  <h1>Order lookup</h1>
  <button id="lookup" onclick="document.getElementById('status').textContent='looked up'">Look up order</button>
  <p id="status">not looked up</p>
</body></html>`

var (
	refPattern   = regexp.MustCompile(`"ref"\s*:\s*"(e\d+)"`)
	tabIDPattern = regexp.MustCompile(`"tab":\{"id":"([0-9A-Fa-f]+)"`)
)

func TestSiteConsentIsEnforcedAgainstAProviderBackedBrowser(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, remoteConsentFixture)
	}))
	defer site.Close()
	parsed, err := url.Parse(site.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin := parsed.Scheme + "://" + parsed.Host

	manager := standInRemoteManager(t)
	srv, guard := newConsentServer(t, manager, siteconsent.AdminConfig{})
	srv.SetIdentity(brwidentity.Identity{Transport: brwidentity.TransportOffHostCDP, Mode: "browser-provider"})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	response := callConsentTool(t, srv, "brw_open", map[string]any{"url": site.URL})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("an un-granted origin opened on the provider's browser: %s", response)
	}
	tabs, err := manager.ListTabs(ctx)
	if err != nil {
		t.Fatalf("list the provider's tabs: %v", err)
	}
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, origin) {
			t.Fatalf("the provider's browser has a tab at %q despite the consent refusal", tab.URL)
		}
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: origin, Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response = callConsentTool(t, srv, "brw_open", map[string]any{"url": site.URL})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("a granted origin was refused: %s", response)
	}
	match := tabIDPattern.FindStringSubmatch(response)
	if match == nil {
		t.Fatalf("brw_open returned no tab id: %s", response)
	}

	opened := match[1]
	pageCtx := browser.WithTabID(ctx, opened)

	response = callConsentTool(t, srv, "brw_find", map[string]any{"query": "Look up order", "tab_id": opened})
	found := refPattern.FindStringSubmatch(response)
	if found == nil {
		t.Fatalf("brw_find on the provider's browser returned no ref: %s", response)
	}
	ref := found[1]

	response = callConsentTool(t, srv, "brw_click", map[string]any{"ref": ref, "tab_id": opened})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("a read-only grant acted on the provider's browser: %s", response)
	}
	if status := pageStatus(t, pageCtx, manager); status != "not looked up" {
		t.Fatalf("the refused click ran anyway: the page says %q", status)
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: origin, Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response = callConsentTool(t, srv, "brw_click", map[string]any{"ref": ref, "tab_id": opened})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("a granted action was refused on the provider's browser: %s", response)
	}
	if status := pageStatus(t, pageCtx, manager); status != "looked up" {
		t.Fatalf("the granted click never reached the provider's browser: the page says %q", status)
	}
}

func pageStatus(t *testing.T, ctx context.Context, manager *browser.Manager) string {
	t.Helper()
	value, err := manager.Evaluate(ctx, `document.getElementById('status').textContent`)
	if err != nil {
		t.Fatalf("read the fixture's status line: %v", err)
	}
	text, _ := value.(string)
	return text
}

func TestSessionStateIsRefusedOnAProviderBackedBrowser(t *testing.T) {
	manager := standInRemoteManager(t)
	srv := New(manager)
	srv.SetIdentity(brwidentity.Identity{Transport: brwidentity.TransportOffHostCDP, Mode: "browser-provider"})

	if advertisedNames(srv)["brw_state"] {
		t.Error("brw_state is advertised on a provider-backed daemon")
	}
	for _, args := range []map[string]any{
		{"action": "list"},
		{"action": "restore", "snapshot_id": "st_0123456789abcdef0123456789abcdef", "origins": []any{"https://app.example.com"}},
		{"action": "save", "origins": []any{"https://app.example.com"}},
		{"action": "delete", "snapshot_id": "st_0123456789abcdef0123456789abcdef"},
	} {
		response := callConsentTool(t, srv, "brw_state", args)
		if !strings.Contains(response, `"isError":true`) {
			t.Errorf("brw_state %v on a provider-backed browser = %s, want a refusal", args["action"], response)
		}
		if !strings.Contains(response, "not on this machine") {
			t.Errorf("brw_state %v refusal = %s, want it to name the capability class", args["action"], response)
		}
	}
}
