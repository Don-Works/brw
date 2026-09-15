//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/plugin"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/target"
)

// The stand-in.
//
// brw has no Browserbase or Browserless account and this suite will not create
// one. It does not need to: a second headless Chrome started on this machine
// with --remote-debugging-port publishes a CDP websocket URL that is, from
// brw's side of the socket, indistinguishable from a hosted one. Same protocol,
// same handshake, same "a browser brw did not launch and whose filesystem is
// not this process's".
//
// What that does NOT prove is any provider's auth or session API. The plugin
// contract is exercised end to end — manifest, capability grant, credential by
// reference, envelope, teardown — against a mint script standing in for the
// operator's own program. Whether a given vendor's API returns what that script
// returns is the operator's business, which is the whole reason the capability
// carries the provider instead of brw shipping six of them.
func standInEndpoint(t *testing.T) string {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	launcher, err := cdp.Launch(ctx, cdp.LaunchConfig{UserDataDir: t.TempDir(), Headless: true})
	if err != nil {
		t.Skipf("stand-in Chrome did not start: %v", err)
	}
	t.Cleanup(func() { _ = launcher.Close() })

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, launcher.Endpoint()+"/json/version", nil)
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
	return version.WebSocketDebuggerURL
}

// standInProviderManager builds the WHOLE lane: a plugin manifest on disk, a
// capability grant, a mint program, an envelope, a manager over the session it
// named, and a teardown that runs at Close. Nothing here shortcuts to
// Config.Remote with a hand-written struct, because the shortcut would not
// exercise the half of this work that is the plugin contract.
func standInProviderManager(t *testing.T) (*Manager, string) {
	t.Helper()
	wsURL := standInEndpoint(t)
	scripts := t.TempDir()
	root := t.TempDir()
	teardownLog := filepath.Join(scripts, "released")

	mint := filepath.Join(scripts, "mint")
	envelope := fmt.Sprintf(`{"websocket_url":"%s","session_id":"standin-1","expires_in_ms":600000}`, wsURL)
	if err := os.WriteFile(mint, []byte("#!/bin/sh\ncat <<'EOF'\n"+envelope+"\nEOF\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	teardown := filepath.Join(scripts, "teardown")
	if err := os.WriteFile(teardown, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \""+teardownLog+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{mint, teardown} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	manifest := map[string]any{
		"schema_version": plugin.ManifestSchemaVersion,
		"id":             "local.standin",
		"name":           "Local stand-in browser",
		"version":        "1.0.0",
		"description":    "Mints a CDP endpoint against a browser on this machine",
		"capabilities":   []string{plugin.CapabilityBrowserProvider},
		"browser": map[string]any{
			"kind":     plugin.BrowserKindExec,
			"command":  []string{mint},
			"teardown": []string{teardown, plugin.SessionToken},
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "standin.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := plugin.Load(root)
	if err != nil {
		t.Fatalf("load the plugin directory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, release, err := registry.OpenBrowserSession(ctx)
	if err != nil {
		t.Fatalf("open a session from the browser.provider plugin: %v", err)
	}
	manager, err := New(context.Background(), Config{
		Timeout: 30 * time.Second,
		Remote: &RemoteTarget{
			WebSocketURL: session.Endpoint.Reveal(),
			RedactedURL:  session.Endpoint.String(),
			ProviderID:   session.ProviderID,
			SessionID:    session.SessionID,
			ExpiresAt:    time.Now().Add(session.Lifetime),
			Release:      release,
		},
	})
	if err != nil {
		_ = release(ctx)
		t.Fatalf("drive the provider's browser: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close the provider-backed manager: %v", err)
		}
	})
	return manager, teardownLog
}

const remoteScenarioFixture = `<!doctype html><html><head><meta charset="utf-8"><title>remote scenario</title></head>
<body>
  <h1>Order lookup</h1>
  <label for="order">Order reference</label><input id="order" name="order" type="text">
  <button id="lookup" onclick="document.getElementById('status').textContent='Found order ' + document.getElementById('order').value">Look up</button>
  <p id="status">No order looked up</p>
</body></html>`

// Acceptance 1: one reference provider drives a full scenario end to end.
func TestProviderBackedBrowserDrivesAFullScenario(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, remoteScenarioFixture)
	}))
	defer site.Close()

	manager, teardownLog := standInProviderManager(t)
	if !manager.Remote() {
		t.Fatal("a provider-backed manager must report itself as remote")
	}
	session, ok := manager.RemoteSession()
	if !ok || session.ProviderID != "local.standin" || session.SessionID != "standin-1" {
		t.Fatalf("RemoteSession() = %+v %v", session, ok)
	}
	if strings.Contains(session.Endpoint, "/devtools/") {
		t.Fatalf("the reportable endpoint %q carries the path that authenticates the session", session.Endpoint)
	}
	if session.ExpiresAt.IsZero() {
		t.Fatal("RemoteSession() reported no expiry, so /health cannot say when the browser stops existing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	opened, err := manager.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	page, err := manager.Read(tabCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if page.Title != "remote scenario" || !strings.Contains(page.Main, "Order lookup") {
		t.Fatalf("read the provider's page as title=%q main=%q", page.Title, page.Main)
	}

	found, err := manager.Find(tabCtx, snapshot.FindOptions{Query: "Order reference", Role: "textbox"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found.Elements) != 1 {
		t.Fatalf("find matched %d elements", len(found.Elements))
	}
	if _, err := manager.Fill(tabCtx, snapshot.FillOptions{Ref: found.Elements[0].Ref, Text: "fixture-order-7"}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	button, err := manager.Find(tabCtx, snapshot.FindOptions{Query: "Look up", Role: "button"})
	if err != nil || len(button.Elements) != 1 {
		t.Fatalf("find the button: %v (%d matches)", err, len(button.Elements))
	}
	if _, err := manager.Click(tabCtx, button.Elements[0].Ref); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := manager.WaitFor(tabCtx, "fn:document.getElementById('status').textContent.indexOf('fixture-order-7') >= 0", 15*time.Second); err != nil {
		t.Fatalf("the provider's browser never showed the result: %v", err)
	}
	shot, err := manager.Screenshot(tabCtx)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if len(shot.Data) == 0 {
		t.Fatal("screenshot returned no bytes")
	}

	// The refusals hold on the same live session, not only on a bare struct.
	// Matched on the sentinel every caller branches on rather than on words in
	// the sentence: this assertion read the message and broke when the reason
	// was reworded, which tells nobody anything about the gate.
	if _, err := manager.Downloads(tabCtx); !errors.Is(err, ErrRemoteTargetUnsupported) {
		t.Fatalf("Downloads on a live provider session = %v, want the named capability refusal", err)
	}
	if err := manager.CheckProfileSession(); !errors.Is(err, ErrRemoteTargetUnsupported) {
		t.Fatalf("CheckProfileSession on a live provider session = %v, want the named capability refusal", err)
	}

	// Closing hands the browser back, aimed at the session that was opened.
	if err := manager.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	released, err := os.ReadFile(teardownLog)
	if err != nil {
		t.Fatalf("the teardown never ran, so the provider still holds the browser: %v", err)
	}
	if string(released) != "standin-1" {
		t.Fatalf("the teardown was handed %q, want the opened session id", released)
	}
}

// Acceptance 2, the containment half: a remote target is LESS trusted than a
// local one, so --allowed-domains has to be the same confinement boundary
// there. The page is served from an allowlisted host and reaches for a
// subresource on one that is not.
func TestContainmentHoldsOnAProviderBackedBrowser(t *testing.T) {
	// Atomic because it is written from httptest's handler goroutine and read
	// from the test goroutine, and it is only ever written when containment has
	// already failed - so an unsynchronised counter would turn the regression
	// this test exists to report into a race report under -race.
	var offAllowlistHits atomic.Int64
	offAllowlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		offAllowlistHits.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "leaked")
	}))
	defer offAllowlist.Close()
	// httptest binds 127.0.0.1; the policy matches by HOST, so the off-list
	// origin is addressed by a different hostname on the same interface.
	offAllowlistURL := asLocalhost(offAllowlist.URL)

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><body>
			<p>contained</p>
			<script>
			  window.__results = {};
			  fetch(%q).then(function(r){ return r.text(); })
			    .then(function(t){ window.__results.fetch = 'reached:' + t; })
			    .catch(function(e){ window.__results.fetch = 'blocked'; });
			</script>
			</body></html>`, offAllowlistURL+"/exfil")
	}))
	defer allowed.Close()

	manager, _ := standInProviderManager(t)
	manager.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, allowed.URL)}})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var id target.ID
	if err := manager.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	tabID := string(id)
	manager.refs.SetActive(tabID)
	if _, err := manager.tabContext(tabID); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := manager.NavigateTo(ctx, allowed.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := manager.WaitFor(ctx, "fn:window.__results && window.__results.fetch !== undefined", 15*time.Second); err != nil {
		t.Fatalf("the page never recorded a fetch outcome: %v", err)
	}
	value, err := manager.Evaluate(ctx, `window.__results.fetch`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got, _ := value.(string); got != "blocked" {
		t.Errorf("a page on the provider's browser fetched an off-allowlist host: %q", got)
	}
	if hits := offAllowlistHits.Load(); hits != 0 {
		t.Errorf("the off-allowlist origin was reached %d times from the provider's browser", hits)
	}
	blocked := manager.BlockedRequests(tabID)
	found := false
	for _, entry := range blocked {
		if strings.Contains(entry.URL, "/exfil") && strings.Contains(entry.Reason, "allowlist") {
			found = true
		}
	}
	if !found {
		t.Errorf("containment recorded %+v; the refusal has to be reportable on a remote target too", blocked)
	}

	// And the navigation half: the agent-facing check refuses the same host.
	if _, err := manager.NavigateTo(ctx, offAllowlistURL+"/exfil"); err == nil {
		t.Error("navigating the provider's browser to an off-allowlist host succeeded")
	}
}
