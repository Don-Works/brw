package extensionbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	brwcdp "github.com/Don-Works/brw/internal/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type shippedManifest struct {
	Permissions             []string `json:"permissions"`
	HostPermissions         []string `json:"host_permissions"`
	OptionalHostPermissions []string `json:"optional_host_permissions"`
}

func (m shippedManifest) grantableHosts() []string {
	return append(append([]string{}, m.HostPermissions...), m.OptionalHostPermissions...)
}

func shippedExtensionManifest(t *testing.T) shippedManifest {
	t.Helper()
	var manifest shippedManifest
	raw, err := os.ReadFile(filepath.Join("..", "..", "extension", "manifest.json"))
	if err != nil {
		t.Fatalf("read the shipped extension manifest: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse the shipped extension manifest: %v", err)
	}
	return manifest
}

func TestShippedExtensionHoldsNoHostAccessForADeclarativeRedirect(t *testing.T) {
	manifest := shippedExtensionManifest(t)
	for _, permission := range manifest.Permissions {
		if permission == "declarativeNetRequestWithHostAccess" {
			t.Errorf("the extension now requests %q, which is the permission a redirect rule needs; revisit the redirect row in docs/install.md and browser.ErrRouteRedirectUnsupported", permission)
		}
	}
	if len(manifest.HostPermissions) == 0 {
		t.Fatal("the shipped manifest declares no host_permissions at all; this test is measuring nothing")
	}
	for _, entry := range manifest.grantableHosts() {
		if !loopbackHostPattern(entry) {
			t.Errorf("host pattern %q is not loopback-only; a declarativeNetRequest redirect can fire for that host once it is granted, so the redirect row in docs/install.md and browser.ErrRouteRedirectUnsupported no longer hold", entry)
		}
	}
}

func loopbackHostPattern(pattern string) bool {
	parsed, err := url.Parse(strings.Replace(pattern, "*.", "", 1))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	default:
		return false
	}
}

func TestDeclarativeNetRequestRedirectNeverFiresUnderShippedPermissions(t *testing.T) {
	manifest := shippedExtensionManifest(t)
	const interceptedHost = "notlocal.test"
	for _, tc := range []struct {
		name              string
		extraHosts        []string
		subjectOnLoopback bool
		requestOnLoopback bool
		wantRedirect      bool
	}{
		{
			name:         "neither the request url nor the initiator is granted",
			wantRedirect: false,
		},
		{
			name:         "both the request url and the initiator are granted",
			extraHosts:   []string{"http://" + interceptedHost + "/*"},
			wantRedirect: true,
		},
		{

			name:              "the request url is granted and the initiator is not",
			requestOnLoopback: true,
			wantRedirect:      false,
		},
		{

			name:              "the request url is granted and so is the initiator",
			subjectOnLoopback: true,
			requestOnLoopback: true,
			wantRedirect:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runDNRRedirectProbe(t, dnrProbeInput{
				permissions:       manifest.Permissions,
				hostPermissions:   append(append([]string{}, manifest.HostPermissions...), tc.extraHosts...),
				interceptedHost:   interceptedHost,
				subjectOnLoopback: tc.subjectOnLoopback,
				requestOnLoopback: tc.requestOnLoopback,
			})

			if result.blocked != "ERR" {
				t.Fatalf("the block control returned %q rather than failing; the rules never reached Chrome, so nothing here is measuring permissions", result.blocked)
			}
			if tc.wantRedirect && result.redirected != "STUB" {
				t.Fatalf("with host access to both the request url and the initiator the redirect still did not fire (page saw %q); the matching negative case would then be passing for some other reason", result.redirected)
			}
			if !tc.wantRedirect && result.redirected != "REAL" {
				t.Fatalf("the redirect fired without host access to both the request url and its initiator (page saw %q); the bridge could now offer brw_route behaviour=redirect, so revisit browser.ErrRouteRedirectUnsupported and the matrix in docs/install.md", result.redirected)
			}
		})
	}
}

type dnrProbeResult struct {
	redirected string
	blocked    string
}

type dnrProbeInput struct {
	permissions     []string
	hostPermissions []string
	interceptedHost string

	subjectOnLoopback bool

	requestOnLoopback bool
}

func runDNRRedirectProbe(t *testing.T, in dnrProbeInput) dnrProbeResult {
	t.Helper()
	permissions, hostPermissions, interceptedHost := in.permissions, in.hostPermissions, in.interceptedHost
	var mu sync.Mutex
	reports := map[string]string{}
	reported := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()

	mux.HandleFunc("/installed", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reports[r.URL.Query().Get("rules")] = r.URL.Query().Get("outcome")
		complete := len(reports) == 2
		mu.Unlock()
		if complete {
			once.Do(func() { close(reported) })
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "ok")
	})
	for path, body := range map[string]string{"/stub": "STUB", "/intercepted": "REAL", "/blocked": "REAL"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			fmt.Fprint(w, body)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	loopback := "http://127.0.0.1:" + port
	offPermission := "http://" + interceptedHost + ":" + port

	subjectBase, requestBase := offPermission, offPermission
	if in.subjectOnLoopback {
		subjectBase = loopback
	}
	if in.requestOnLoopback {
		requestBase = loopback
	}

	mux.HandleFunc("/subject", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>
		  window.__out = {};
		  function probe(name, url) {
		    return fetch(url).then(function(r){ return r.text(); })
		      .then(function(t){ window.__out[name] = t; })
		      .catch(function(){ window.__out[name] = 'ERR'; });
		  }
		  Promise.all([probe('redirected', %q), probe('blocked', %q)])
		    .then(function(){ window.__done = true; });
		</script></body></html>`, requestBase+"/intercepted", requestBase+"/blocked")
	})

	resourceTypes := []string{"main_frame", "sub_frame", "xmlhttprequest", "script", "image", "other"}
	blockRules := []map[string]any{{
		"id": 1, "priority": 1,
		"action":    map[string]any{"type": "block"},
		"condition": map[string]any{"urlFilter": "/blocked", "resourceTypes": resourceTypes},
	}}
	redirectRules := []map[string]any{{
		"id": 2, "priority": 1,
		"action":    map[string]any{"type": "redirect", "redirect": map[string]any{"url": loopback + "/stub"}},
		"condition": map[string]any{"urlFilter": "/intercepted", "resourceTypes": resourceTypes},
	}}
	blockJSON, err := json.Marshal(blockRules)
	if err != nil {
		t.Fatalf("encode the block rule: %v", err)
	}
	redirectJSON, err := json.Marshal(redirectRules)
	if err != nil {
		t.Fatalf("encode the redirect rule: %v", err)
	}
	manifestJSON, err := json.Marshal(map[string]any{
		"manifest_version": 3,
		"name":             "brw redirect capability probe",
		"version":          "1.0",
		"permissions":      permissions,
		"host_permissions": hostPermissions,
		"background":       map[string]any{"service_worker": "sw.js"},
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	extensionDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extensionDir, "manifest.json"), manifestJSON, 0o600); err != nil {
		t.Fatalf("write probe manifest: %v", err)
	}
	worker := fmt.Sprintf(`
const REPORT = %q;
async function install(name, rules) {
  try {
    await chrome.declarativeNetRequest.updateSessionRules({ addRules: rules, removeRuleIds: [] });
    const live = await chrome.declarativeNetRequest.getSessionRules();
    const ids = new Set(live.map((rule) => rule.id));
    const listed = rules.every((rule) => ids.has(rule.id));
    await fetch(REPORT + "?rules=" + name + "&outcome=" + (listed ? "accepted" : "accepted-but-not-listed"));
  } catch (error) {
    await fetch(REPORT + "?rules=" + name + "&outcome=refused:" + encodeURIComponent(String((error && error.message) || error)));
  }
}
install("block", %s).then(() => install("redirect", %s));
`, loopback+"/installed", string(blockJSON), string(redirectJSON))
	if err := os.WriteFile(filepath.Join(extensionDir, "sw.js"), []byte(worker), 0o600); err != nil {
		t.Fatalf("write probe service worker: %v", err)
	}

	pipe, stop := launchCDPPipeBrowser(t, "--host-resolver-rules=MAP "+interceptedHost+" 127.0.0.1")
	defer stop()
	if _, err := pipe.call("Extensions.loadUnpacked", map[string]any{"path": extensionDir}); err != nil {

		t.Skipf("this Chrome cannot load an unpacked extension over CDP (%v); the redirect capability claim is unverified on this machine", err)
	}
	select {
	case <-reported:
	case <-time.After(45 * time.Second):
		mu.Lock()
		seen := fmt.Sprint(reports)
		mu.Unlock()
		t.Fatalf("the probe extension loaded but never reported both rule sets (%s); the measurement below would be reading an unarmed browser", seen)
	}
	mu.Lock()
	blockOutcome, redirectOutcome := reports["block"], reports["redirect"]
	mu.Unlock()
	if blockOutcome != "accepted" {
		t.Fatalf("Chrome answered %q for the block rule, which needs no host access; nothing here is measuring permissions", blockOutcome)
	}

	if redirectOutcome != "accepted" {
		t.Fatalf("Chrome answered %q for the redirect rule; docs/install.md says such a rule is accepted and listed, then never applied", redirectOutcome)
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), pipe.wsURL)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 40*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, _, _, _, err := page.Navigate(subjectBase + "/subject").Do(c)
		return err
	})); err != nil {
		t.Fatalf("navigate to the subject page: %v", err)
	}
	var out map[string]string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var done bool
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`window.__done === true`, &done))
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__out`, &out)); err != nil {
		t.Fatalf("read the page's results: %v", err)
	}
	return dnrProbeResult{redirected: out["redirected"], blocked: out["blocked"]}
}

type cdpPipeBrowser struct {
	toBrowser   *os.File
	fromBrowser *bufio.Reader
	wsURL       string
	nextID      int
}

func launchCDPPipeBrowser(t *testing.T, extraArgs ...string) (*cdpPipeBrowser, func()) {
	t.Helper()
	chromePath, err := brwcdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	commandsRead, commandsWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("open the command pipe: %v", err)
	}
	eventsRead, eventsWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("open the event pipe: %v", err)
	}

	userDataDir, err := os.MkdirTemp("", "brw-dnr-probe-")
	if err != nil {
		t.Fatalf("create a browser profile directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(userDataDir) })
	args := append([]string{
		"--remote-debugging-pipe",

		"--remote-debugging-port=0",
		"--user-data-dir=" + userDataDir,
		"--headless=new",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--enable-unsafe-extension-debugging",
	}, extraArgs...)
	command := exec.Command(chromePath, args...)

	command.ExtraFiles = []*os.File{commandsRead, eventsWrite}
	command.Stderr = io.Discard
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		t.Skipf("Chrome did not start: %v", err)
	}
	_ = commandsRead.Close()
	_ = eventsWrite.Close()

	pipe := &cdpPipeBrowser{toBrowser: commandsWrite, fromBrowser: bufio.NewReaderSize(eventsRead, 1<<20)}
	stop := func() {
		_ = commandsWrite.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		_ = eventsRead.Close()
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(userDataDir, "DevToolsActivePort"))
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) >= 2 {
				pipe.wsURL = fmt.Sprintf("ws://127.0.0.1:%s%s", lines[0], lines[1])
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pipe.wsURL == "" {
		stop()
		t.Skip("Chrome never published a DevTools endpoint")
	}
	return pipe, stop
}

func (b *cdpPipeBrowser) call(method string, params map[string]any) (json.RawMessage, error) {
	b.nextID++
	id := b.nextID
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if _, err := b.toBrowser.Write(append(encoded, 0)); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		line, err := b.fromBrowser.ReadBytes(0)
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line[:len(line)-1], &reply); err != nil || reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		return reply.Result, nil
	}
	return nil, fmt.Errorf("%s: no reply", method)
}
