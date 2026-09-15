package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// This file measures one thing against a real browser: what reaches a loopback
// daemon when an MV3 service worker fetches it, and what reaches the same
// daemon when a web page on another site causes a request to it.
//
// It exists because tokenServable serves the handshake token to a caller that
// sends NO Origin, and the justification for that is a browser behaviour rather
// than an argument. A browser behaviour can change, and a comment asserting one
// gives the next reader no way to tell whether it is still true.

// mv3ProbePath is the path every probe fetches. It is not /status: the
// measurement wants the request headers, and standing up a real bridge to serve
// them a token would put a secret in a fixture for nothing.
const mv3ProbePath = "/mv3-probe"

// The callers measured. Each names itself in the query string, because the
// whole question is which of them can be told apart by what arrives.
const (
	callerWorker        = "mv3-service-worker"
	callerExtensionPage = "extension-page"
	callerPageNoCORS    = "web-page-no-cors-fetch"
	callerPageScript    = "web-page-script-element"
)

// mv3MeasuredHeaders are the request properties the empty-Origin decision turns
// on. Sec-Fetch-* is in the list because those are the headers a browser sets
// itself and forbids page script from overriding — the only class of request
// property that says anything about who initiated a BROWSER request.
var mv3MeasuredHeaders = []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"}

type mv3Observation struct {
	headers map[string]string
	host    string
}

func mv3Observe(r *http.Request) mv3Observation {
	observed := mv3Observation{headers: map[string]string{}, host: r.Host}
	for _, name := range mv3MeasuredHeaders {
		observed.headers[name] = r.Header.Get(name)
	}
	return observed
}

func (o mv3Observation) render() string {
	parts := make([]string, 0, len(mv3MeasuredHeaders)+1)
	parts = append(parts, "Host: "+o.host)
	for _, name := range mv3MeasuredHeaders {
		value := o.headers[name]
		if value == "" {
			value = "(absent)"
		}
		parts = append(parts, name+": "+value)
	}
	return strings.Join(parts, ", ")
}

// request rebuilds the measured call as an http.Request, so the production
// guard can be asked about the real thing rather than about a hand-written
// approximation of it.
func (o mv3Observation) request() *http.Request {
	req := httptest.NewRequest(http.MethodGet, mv3ProbePath, nil)
	req.Host = o.host
	for name, value := range o.headers {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	return req
}

// mv3Recorder collects one observation per caller.
type mv3Recorder struct {
	mu      sync.Mutex
	seen    map[string]mv3Observation
	arrived chan string
}

func newMV3Recorder() *mv3Recorder {
	return &mv3Recorder{seen: map[string]mv3Observation{}, arrived: make(chan string, 32)}
}

func (r *mv3Recorder) handle(w http.ResponseWriter, req *http.Request) {
	caller := req.URL.Query().Get("caller")
	r.mu.Lock()
	if _, ok := r.seen[caller]; !ok {
		r.seen[caller] = mv3Observe(req)
	}
	r.mu.Unlock()
	select {
	case r.arrived <- caller:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"connected":false}`)
}

func (r *mv3Recorder) get(caller string) (mv3Observation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	observed, ok := r.seen[caller]
	return observed, ok
}

// await blocks until caller has been seen or the deadline passes.
func (r *mv3Recorder) await(caller string, timeout time.Duration) (mv3Observation, bool) {
	deadline := time.After(timeout)
	for {
		if observed, ok := r.get(caller); ok {
			return observed, true
		}
		select {
		case <-r.arrived:
		case <-deadline:
			observed, ok := r.get(caller)
			return observed, ok
		}
	}
}

// mv3ProbeWait bounds how long one browser gets to start its worker and make
// the call. A browser that ignores --load-extension never will, so this is the
// cost of trying it rather than a race with a slow one.
const mv3ProbeWait = 30 * time.Second

// installedBrowsers lists the Chrome/Chromium builds present on this machine,
// unbranded ones first.
//
// The order is not cdp.Candidates'. Branded Chrome 137+ ignores
// --load-extension (docs/install.md records it, and it is why brwd --extension
// is documented as a Chromium path), so trying it first spends the probe window
// on a browser that cannot answer — and starts Google's separate updater
// process, which inherits this test binary's stderr and can outlive it, which
// `go test` reports as "Test I/O incomplete" and fails the whole package on.
// It is still tried, last, so a machine with only branded Chrome installed gets
// a measurement rather than a skip if that ever changes.
func installedBrowsers() []string {
	var unbranded, branded []string
	for _, candidate := range cdp.Candidates(runtime.GOOS) {
		path := candidate
		if !filepath.IsAbs(candidate) {
			resolved, err := exec.LookPath(candidate)
			if err != nil {
				continue
			}
			path = resolved
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(filepath.Base(path)), "google chrome") ||
			strings.HasPrefix(strings.ToLower(filepath.Base(path)), "google-chrome") {
			branded = append(branded, path)
			continue
		}
		unbranded = append(unbranded, path)
	}
	return append(unbranded, branded...)
}

// quietLaunchArgs keep a probe browser from doing anything but the one fetch
// being measured. The background-networking switches are why they are here: an
// update check spawns a process that inherits this binary's stderr and outlives
// the browser it was started from.
func quietLaunchArgs() []string {
	return []string{
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--no-service-autorun",
	}
}

// browserVersion asks the binary what it is, so the recorded measurement names
// the build it was taken on rather than "the browser on some machine".
func browserVersion(t *testing.T, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		t.Fatalf("read the browser version: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// writeProbeExtension lays down the smallest MV3 extension that reproduces the
// two calls the real extension makes to the daemon: the service worker's fetch
// (fetchBridgeToken) and an extension page's fetch (the options page reading
// /consent).
func writeProbeExtension(t *testing.T, probeURL, otherSiteURL string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"manifest_version": 3,
		"name":             "brw status header probe",
		"version":          "1.0",
		// The host permission is the whole point: it is what makes this fetch
		// the privileged one the real extension makes rather than an ordinary
		// cross-origin request CORS would govern.
		"host_permissions": []string{"http://127.0.0.1/*"},
		"background":       map[string]any{"service_worker": "sw.js", "type": "module"},
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// cache: "no-store" and nothing else, exactly as fetchBridgeToken calls it.
	// A probe that set mode or headers would measure the options it chose.
	// Headless Chrome accepts exactly one startup URL, so the worker opens the
	// other two callers itself rather than the launch command line carrying them.
	worker := fmt.Sprintf(`const PROBE = %q;
const OTHER_SITE = %q;
async function probe(caller) {
  for (let attempt = 0; attempt < 20; attempt++) {
    try {
      await fetch(PROBE + "?caller=" + caller, { cache: "no-store" });
      return;
    } catch (err) {
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
}
probe(%q).then(function () {
  chrome.tabs.create({ url: chrome.runtime.getURL("page.html") });
  chrome.tabs.create({ url: OTHER_SITE });
});
`, probeURL, otherSiteURL, callerWorker)
	if err := os.WriteFile(filepath.Join(dir, "sw.js"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	const page = `<!doctype html><title>probe</title><script src="page.js"></script>`
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	pageScript := fmt.Sprintf(`fetch(%q + "?caller=%s", { cache: "no-store" }).catch(() => {});`, probeURL, callerExtensionPage)
	if err := os.WriteFile(filepath.Join(dir, "page.js"), []byte(pageScript), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// attackerPage is a page on another site that causes requests to the daemon
// without being able to set a header on them. Both shapes reach a loopback Host
// with no Origin, which is the case tokenServable used to accept.
const attackerPage = `<!doctype html><title>other site</title>
<script>
fetch(%q, { mode: "no-cors", cache: "no-store" }).catch(function () {});
var element = document.createElement("script");
element.src = %q;
document.head.appendChild(element);
</script>`

// TestMV3ServiceWorkerAndWebPageStatusHeadersAreMeasured is the measurement
// behind tokenServable's empty-Origin case, taken against a real browser, and
// then fed back through the real guard.
//
// It fails if the browser starts sending an Origin on the worker's fetch (at
// which point the empty-Origin case can be closed and this is the evidence for
// doing it), if the worker's call stops reaching the daemon at all, or if the
// guard's verdict on the measured headers changes.
func TestMV3ServiceWorkerAndWebPageStatusHeadersAreMeasured(t *testing.T) {
	browsers := installedBrowsers()
	if len(browsers) == 0 {
		t.Skip("no Chrome/Chromium build is installed")
	}
	// Logged before anything launches, so the run says which builds were
	// available as well as which one the measurement came from. docs/auth-model.md
	// records a branded-Chrome observation this test does not reproduce, and
	// this is what lets a reader see that it was not asked to.
	t.Logf("installed browsers, in the order they are tried: %s", strings.Join(browsers, ", "))

	recorder := newMV3Recorder()
	mux := http.NewServeMux()
	mux.HandleFunc(mv3ProbePath, recorder.handle)
	mux.HandleFunc("/other-site", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Served on the SAME listener, reached through a different hostname, so
		// the page's site differs from the target's. A second listener on
		// 127.0.0.1 would only be a different port, which is the same site.
		target := "http://127.0.0.1:" + portOf(t, r.Host) + mv3ProbePath
		fmt.Fprintf(w, attackerPage, target+"?caller="+callerPageNoCORS, target+"?caller="+callerPageScript)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// httptest binds 127.0.0.1, which is the host the extension holds a
	// permission for. Anything else would measure a request the real extension
	// never makes.
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("fixture served at %s, want a 127.0.0.1 loopback origin", srv.URL)
	}
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	probeURL := srv.URL + mv3ProbePath
	otherSiteURL := "http://localhost:" + port + "/other-site"
	extension := writeProbeExtension(t, probeURL, otherSiteURL)

	// Branded Chrome 137+ ignores --load-extension (docs/install.md says so, and
	// it is why brwd --extension is documented as a Chromium path), so the
	// measurement is taken on the first installed build that actually runs the
	// worker rather than on whichever binary happens to come first.
	var version string
	for _, browser := range browsers {
		build := browserVersion(t, browser)
		profile := browsertest.NewProfile(t)
		launcher, err := cdp.Launch(context.Background(), cdp.LaunchConfig{
			ChromePath:  browser,
			UserDataDir: profile.Dir(),
			Extensions:  []string{extension},
			Headless:    true,
			Args:        quietLaunchArgs(),
		})
		if err != nil {
			t.Logf("%s did not start headless: %v", build, err)
			continue
		}
		if _, ok := recorder.await(callerWorker, mv3ProbeWait); ok {
			version = build
			// The other three callers are driven by the same browser run; give
			// them a moment to land now that it is up.
			recorder.await(callerExtensionPage, 15*time.Second)
			recorder.await(callerPageNoCORS, 15*time.Second)
			recorder.await(callerPageScript, 15*time.Second)
		} else {
			t.Logf("%s loaded no MV3 worker that reached %s within %s", build, probeURL, mv3ProbeWait)
		}
		profile.StopWith(func() { _ = launcher.Close() })
		if version != "" {
			break
		}
	}
	if version == "" {
		t.Fatalf("no installed browser ran an MV3 service worker against %s, so the empty-Origin decision could not be re-measured", probeURL)
	}

	// The comparator: a plain local HTTP client, which is what "curl" means
	// here. It reaches the same handler through the same loopback address.
	if _, err := http.Get(probeURL + "?caller=local-process"); err != nil {
		t.Fatalf("local client request: %v", err)
	}
	local, ok := recorder.await("local-process", 10*time.Second)
	if !ok {
		t.Fatal("the local client's request never arrived")
	}

	t.Logf("browser: %s (measurement taken on the first installed build that ran the worker; later ones were not launched)", version)
	t.Logf("local process (curl-equivalent): %s", local.render())
	for _, caller := range []string{callerWorker, callerExtensionPage, callerPageNoCORS, callerPageScript} {
		observed, ok := recorder.get(caller)
		if !ok {
			t.Fatalf("%s never reached the daemon, so its headers are unmeasured", caller)
		}
		t.Logf("%s: %s", caller, observed.render())
	}

	worker, _ := recorder.get(callerWorker)
	if worker.headers["Origin"] != "" {
		t.Fatalf("%s sends Origin %q on an MV3 worker's privileged loopback fetch; tokenServable's empty-Origin case can now be closed",
			version, worker.headers["Origin"])
	}

	// The measured verdicts, run through the real guard. A page on another site
	// reaching a loopback daemon is the caller the Sec-Fetch-Site gate exists
	// for: it sends no Origin, so nothing else in the request distinguishes it
	// from the extension.
	wantServed := map[string]bool{
		callerWorker:        true,
		callerExtensionPage: true,
		callerPageNoCORS:    false,
		callerPageScript:    false,
	}
	bridge := New("", 5*time.Second, profilepolicy.DefaultBridgeExtensionID)
	bridge.SetAuthToken("fixture-bridge-handshake-token-measured")
	for _, caller := range sortedCallers(wantServed) {
		observed, _ := recorder.get(caller)
		if got := bridge.tokenServable(observed.request()); got != wantServed[caller] {
			t.Fatalf("tokenServable(%s measured on %s: %s) = %v, want %v",
				caller, version, observed.render(), got, wantServed[caller])
		}
	}
}

func sortedCallers(from map[string]bool) []string {
	names := make([]string, 0, len(from))
	for name := range from {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func portOf(t *testing.T, hostport string) string {
	t.Helper()
	index := strings.LastIndex(hostport, ":")
	if index < 0 {
		t.Fatalf("no port in host %q", hostport)
	}
	return hostport[index+1:]
}

// TestBridgeCommentsCiteTheMeasuredBrowser keeps the prose honest.
//
// The empty-Origin decision is justified in several places by a named browser
// build. A measurement whose build is not the one the code cites is a
// measurement of something else, so the citations and docs/auth-model.md have
// to name the same build, and the doc has to say when it was checked.
func TestBridgeCommentsCiteTheMeasuredBrowser(t *testing.T) {
	root := repositoryRootForDocs(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "auth-model.md"))
	if err != nil {
		t.Fatalf("read docs/auth-model.md: %v", err)
	}
	// The doc is the record: it must name the build AND the date, because the
	// point of writing a measurement down is that the next reader can tell
	// whether it is stale.
	// Whitespace-tolerant: the doc wraps its prose, so the build and the date
	// can sit on different lines.
	measurement := regexp.MustCompile(`Measured on\s+(Google Chrome|Chromium)\s+([0-9][0-9.]*)\s+on\s+(\d{4}-\d{2}-\d{2})`)
	found := measurement.FindSubmatch(doc)
	if found == nil {
		t.Fatal(`docs/auth-model.md carries no "Measured on <browser> <version> on <YYYY-MM-DD>" line; a browser behaviour recorded without a build and a date cannot be re-checked`)
	}
	build := string(found[1]) + " " + string(found[2])

	for _, file := range []string{
		filepath.Join(root, "internal", "extensionbridge", "bridge_tokenissue.go"),
		filepath.Join(root, "internal", "extensionbridge", "consent.go"),
		filepath.Join(root, "internal", "extensionbridge", "bridge_tokenissue_test.go"),
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(file), err)
		}
		if !strings.Contains(string(source), build) {
			t.Fatalf("%s justifies the empty-Origin case without naming %s, the build docs/auth-model.md records the measurement on",
				filepath.Base(file), build)
		}
	}
}

// repositoryRootForDocs walks up from the package directory to the module root.
func repositoryRootForDocs(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
