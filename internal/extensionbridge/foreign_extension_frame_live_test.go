package extensionbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Chrome behaviour extension/service_worker.js works around when a page
// embeds another extension's frame, measured rather than asserted. Two
// unpacked extensions go into a real browser: "foreign" exposes a page that a
// web page can frame, the way a password manager's inline menu does, and
// "probe" holds the debugger, webNavigation and scripting permissions brw holds,
// with brw's loopback-only host access. The probe attaches to a page, frames the
// foreign page into it, and reports what Chrome then allows.
func TestChromeRefusesTheDebuggerForATabHoldingAForeignExtensionFrame(t *testing.T) {
	var mu sync.Mutex
	reports := map[string]string{}
	done := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		step, outcome, _ := strings.Cut(string(body), " ")
		mu.Lock()
		reports[step] = outcome
		mu.Unlock()
		if step == "done" {
			once.Do(func() { close(done) })
		}
	})
	mux.HandleFunc("/subject", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>subject</title></head><body><input id=email><button>Send</button></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")

	foreignDir := t.TempDir()
	writeProbeFile(t, foreignDir, "manifest.json", `{"manifest_version":3,"name":"foreign autofill menu","version":"1",`+
		`"web_accessible_resources":[{"resources":["menu.html"],"matches":["<all_urls>"]}]}`)
	writeProbeFile(t, foreignDir, "menu.html", `<html><body>saved logins</body></html>`)

	pipe, stop := launchCDPPipeBrowser(t, "--host-resolver-rules=MAP off-permission.test 127.0.0.1")
	defer stop()
	raw, err := pipe.call("Extensions.loadUnpacked", map[string]any{"path": foreignDir})
	if err != nil {
		t.Skipf("this Chrome cannot load an unpacked extension over CDP (%v); the foreign-frame behaviour is unverified on this machine", err)
	}
	var foreign struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &foreign); err != nil || foreign.ID == "" {
		t.Fatalf("Extensions.loadUnpacked returned no id: %s", raw)
	}
	menuURL := "chrome-extension://" + foreign.ID + "/menu.html"

	probeDir := t.TempDir()
	writeProbeFile(t, probeDir, "manifest.json", `{"manifest_version":3,"name":"foreign frame probe","version":"1",`+
		`"permissions":["debugger","tabs","webNavigation","scripting"],"host_permissions":["http://127.0.0.1/*"],`+
		`"background":{"service_worker":"sw.js"}}`)
	writeProbeFile(t, probeDir, "sw.js", fmt.Sprintf(`
const REPORT = "http://127.0.0.1:%[1]s/report";
const MENU = %[2]q;
const report = (step, outcome) => fetch(REPORT, { method: "POST", body: step + " " + outcome });
const message = (error) => String((error && error.message) || error);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
let committed = "";
let detachReason = "";
chrome.webNavigation.onCommitted.addListener((d) => { if (d.frameId !== 0 && d.url === MENU) committed = "reported"; });
chrome.debugger.onDetach.addListener((_source, reason) => { detachReason = reason; });
async function until(check, ms) {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(100)) if (check()) return true;
  return check();
}
async function attempt(step, fn) {
  try { await report(step, "ok " + JSON.stringify(await fn())); } catch (error) { await report(step, "refused " + message(error)); }
}
async function run(host) {
  const tab = await chrome.tabs.create({ url: "http://" + host + ":%[1]s/subject" });
  for (const end = Date.now() + 20000; Date.now() < end; await sleep(100)) {
    if ((await chrome.tabs.get(tab.id)).status === "complete") break;
  }
  const debuggee = { tabId: tab.id };
  await attempt(host + ":attach-clean", () => chrome.debugger.attach(debuggee, "1.3"));
  const frame = "var f=document.createElement('iframe');f.src=" + JSON.stringify(MENU) + ";document.body.appendChild(f);1";
  await chrome.debugger.sendCommand(debuggee, "Runtime.evaluate", { expression: frame }).catch(() => {});
  await until(() => detachReason !== "" && committed !== "", 10000);
  await report(host + ":detached", detachReason || "no");
  await report(host + ":committed", committed || "no");
  await attempt(host + ":send-after", () => chrome.debugger.sendCommand(debuggee, "Runtime.evaluate", { expression: "1", returnByValue: true }));
  await attempt(host + ":reattach", () => chrome.debugger.attach(debuggee, "1.3"));
  await attempt(host + ":attach-by-target", async () => {
    const target = (await chrome.debugger.getTargets()).find((t) => t.tabId === tab.id);
    return chrome.debugger.attach({ targetId: target.id }, "1.3");
  });
  await attempt(host + ":target-listed", async () => (await chrome.debugger.getTargets()).some((t) => t.url === MENU) ? "listed" : "absent");
  await attempt(host + ":all-frames", async () => (await chrome.webNavigation.getAllFrames(debuggee)).map((f) => f.url));
  await attempt(host + ":script-top", async () => (await chrome.scripting.executeScript({
    target: { tabId: tab.id, frameIds: [0] }, world: "MAIN",
    func: (expression) => (0, eval)(expression), args: ["document.title + '|' + document.querySelectorAll('iframe').length"]
  }))[0].result);
  await attempt(host + ":script-all", async () => (await chrome.scripting.executeScript({
    target: { tabId: tab.id, allFrames: true }, world: "MAIN", func: () => location.protocol
  })).map((r) => r.result));
  detachReason = "";
  committed = "";
}
(async () => {
  await run("127.0.0.1");
  await run("off-permission.test");
  await report("done", "");
})();
`, port, menuURL))
	if _, err := pipe.call("Extensions.loadUnpacked", map[string]any{"path": probeDir}); err != nil {
		t.Fatalf("load the probe extension: %v", err)
	}
	select {
	case <-done:
	case <-time.After(150 * time.Second):
		mu.Lock()
		seen := fmt.Sprint(reports)
		mu.Unlock()
		t.Fatalf("the probe never finished (%s)", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	const refusal = "refused Cannot access a chrome-extension:// URL of different extension"
	for _, host := range []string{"127.0.0.1", "off-permission.test"} {
		want := map[string]func(string) bool{
			// The frame committing ends a live session...
			"attach-clean": func(s string) bool { return strings.HasPrefix(s, "ok") },
			"detached":     func(s string) bool { return s == "target_closed" },
			// ...and nothing on the debugger gets back in while it is there.
			"send-after":       func(s string) bool { return s == refusal },
			"reattach":         func(s string) bool { return s == refusal },
			"attach-by-target": func(s string) bool { return s == refusal },
			// brw can see the frame commit and its target, but getAllFrames
			// leaves it out, so the extension has to remember it itself.
			"committed":     func(s string) bool { return s == "reported" },
			"target-listed": func(s string) bool { return s == `ok "listed"` },
			"all-frames":    func(s string) bool { return strings.HasPrefix(s, "ok") && !strings.Contains(s, "chrome-extension://") },
		}
		if host == "127.0.0.1" {
			// chrome.scripting ignores the foreign frame, top frame or all frames,
			// wherever the extension holds host access.
			want["script-top"] = func(s string) bool { return s == `ok "subject|1"` }
			want["script-all"] = func(s string) bool { return s == `ok ["http:"]` }
		} else {
			// Without host access it is refused for the page, as it would be on
			// any site brw's loopback-only manifest does not name.
			want["script-top"] = func(s string) bool {
				return strings.HasPrefix(s, "refused Cannot access contents of url")
			}
		}
		for step, ok := range want {
			got, present := reports[host+":"+step]
			if !present || !ok(got) {
				t.Errorf("%s %s: got %q", host, step, got)
			}
		}
	}
}

func writeProbeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
