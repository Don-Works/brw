package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/setup"
)

// TestDoctorNamesTheLiveBridgeEndpoint is the whole point of the check: the
// packaged bridge-defaults.json and the extension's stored config are
// indistinguishable on disk, so a machine with a stale file and a working stored
// config and a machine that is genuinely broken look identical to anything that
// reads the filesystem. Every case below is decided by what the extension
// reported, and the two file-presence cases prove the file neither forces a red
// nor earns a green on its own.
func TestDoctorNamesTheLiveBridgeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		// packagedPort: "dead" writes a bridge-defaults.json naming a port
		// nothing listens on, "live" writes one naming this profile's bridge,
		// "" writes no file at all.
		packagedFile string
		// reported is what the extension said, and where it said it.
		connected     bool
		helloStatus   string // "live" or "dead"
		helloSource   string
		refusedStatus string // "live" or "dead"
		refusedSource string

		wantStatus   string
		wantDetails  []string
		wantAbsent   []string
		wantReportOK bool
	}{
		{
			name:         "a stale file loses to a working stored config",
			packagedFile: "dead",
			connected:    true,
			helloStatus:  "live",
			helloSource:  "stored",
			wantStatus:   checkOK,
			wantDetails:  []string{"the extension is using", "stored config", "bridge-defaults.json still names", "overridden"},
			wantReportOK: true,
		},
		{
			name:          "a live config pointing at a dead port is red with no file anywhere",
			refusedStatus: "dead",
			refusedSource: "stored",
			wantStatus:    checkFail,
			wantDetails:   []string{"nothing is listening there", "stored config", "refused handshake"},
		},
		{
			name:          "a live config pointing at a dead port names the packaged layer it came from",
			packagedFile:  "dead",
			refusedStatus: "dead",
			refusedSource: "packaged",
			wantStatus:    checkFail,
			wantDetails:   []string{"nothing is listening there", "packaged bridge-defaults.json"},
		},
		{
			name:          "a refused handshake naming this bridge is not an endpoint fault",
			refusedStatus: "live",
			refusedSource: "stored",
			wantStatus:    checkOK,
			wantDetails:   []string{"that is this profile's bridge"},
		},
		{
			name:         "a file naming a dead port is red only because nothing else reported",
			packagedFile: "dead",
			wantStatus:   checkFail,
			wantDetails:  []string{"no extension has reported", "bridge-defaults.json names", "nothing is listening there"},
		},
		{
			name:         "a file naming this bridge is green",
			packagedFile: "live",
			wantStatus:   checkOK,
			wantDetails:  []string{"the installed bridge-defaults.json names this bridge"},
			wantReportOK: false, // bridge_connected is red: nothing is connected.
		},
		{
			name:       "nothing on disk and nothing reported is not a diagnosis",
			wantStatus: checkSkip,
			wantDetails: []string{"no extension has reported", "no installed bridge-defaults.json names one",
				"bridge_connected is the check that says so"},
		},
		{
			// A 0.6.0+ extension whose hello omits status_url. Falling through to
			// the refusal would print a stale URL under "the extension is using"
			// on a green line.
			name:          "a connected extension that names no endpoint does not inherit a stale refusal",
			connected:     true,
			refusedStatus: "dead",
			refusedSource: "stored",
			wantStatus:    checkSkip,
			wantDetails:   []string{"no extension has reported"},
			wantAbsent:    []string{"refused handshake"},
			wantReportOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t)
			deadAddr := freeLoopbackAddr(t)
			liveAddr := strings.TrimPrefix(fx.bridge.URL, "http://")
			resolve := func(which string) string {
				switch which {
				case "live":
					return "http://" + liveAddr + "/status"
				case "dead":
					return "http://" + deadAddr + "/status"
				}
				return ""
			}

			if tc.packagedFile != "" {
				addr := liveAddr
				if tc.packagedFile == "dead" {
					addr = deadAddr
				}
				fx.writeFile(filepath.Join(fx.appDir, "extension", setup.BridgeDefaultsFile),
					`{"bridgeUrl":"ws://`+addr+`/extension"}`)
			}

			fx.status.Connected = tc.connected
			fx.status.Hello.StatusURL = resolve(tc.helloStatus)
			fx.status.Hello.ConfigSource = tc.helloSource
			fx.status.LastHandshake.StatusURL = resolve(tc.refusedStatus)
			fx.status.LastHandshake.ConfigSource = tc.refusedSource

			report := fx.report()
			check := checkByName(t, report, "bridge_config")
			if check.Status != tc.wantStatus {
				t.Fatalf("bridge_config = %s (%s), want %s", check.Status, check.Detail, tc.wantStatus)
			}
			for _, want := range tc.wantDetails {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("bridge_config detail %q does not mention %q", check.Detail, want)
				}
			}
			for _, unwanted := range tc.wantAbsent {
				if strings.Contains(check.Detail, unwanted) {
					t.Fatalf("bridge_config detail %q still mentions %q", check.Detail, unwanted)
				}
			}
			if tc.wantStatus == checkFail && check.Fix == "" {
				t.Fatal("a red bridge_config check printed no command to run")
			}
			if tc.wantReportOK && !report.OK {
				t.Fatalf("machine reported failures: %v", report.Failures)
			}
		})
	}
}

// TestDoctorBridgeConfigPrefersTheLiveHelloOverAStaleRefusal: a refused
// handshake is kept until the daemon restarts, so a reload that fixed the
// endpoint would otherwise keep being reported as broken.
func TestDoctorBridgeConfigPrefersTheLiveHelloOverAStaleRefusal(t *testing.T) {
	fx := newDoctorFixture(t)
	dead := "http://" + freeLoopbackAddr(t) + "/status"
	live := "http://" + strings.TrimPrefix(fx.bridge.URL, "http://") + "/status"

	fx.status.Connected = true
	fx.status.Hello.StatusURL = live
	fx.status.Hello.ConfigSource = "stored"
	fx.status.LastHandshake.StatusURL = dead
	fx.status.LastHandshake.ConfigSource = "packaged"

	check := checkByName(t, fx.report(), "bridge_config")
	if check.Status != checkOK {
		t.Fatalf("bridge_config = %s (%s), want ok", check.Status, check.Detail)
	}
	if strings.Contains(check.Detail, dead) {
		t.Fatalf("a connected extension was reported against a stale refusal: %s", check.Detail)
	}
}

// TestBridgeDefaultsAreReadFromEveryInstalledCopy: an upgrade preserves a
// per-profile copy's own file, so the drift can live in any of them.
func TestBridgeDefaultsAreReadFromEveryInstalledCopy(t *testing.T) {
	appDir := t.TempDir()
	write := func(dir, body string) {
		t.Helper()
		target := filepath.Join(appDir, dir)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, setup.BridgeDefaultsFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("extension", `{"bridgeUrl":"ws://127.0.0.1:17311/extension"}`)
	write("extension-work", `{"statusUrl":"http://localhost:19311/status"}`)
	write("extension-port", `{"bridgePort":19312}`)
	write("extension-label", `{"label":"desk"}`)

	found, err := setup.InstalledBridgeDefaults(appDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range found {
		got[filepath.Base(filepath.Dir(file.Path))] = file.StatusURL
	}
	want := map[string]string{
		"extension":       "http://127.0.0.1:17311/status",
		"extension-work":  "http://localhost:19311/status",
		"extension-port":  "http://127.0.0.1:19312/status",
		"extension-label": "",
	}
	for dir, wantURL := range want {
		if got[dir] != wantURL {
			t.Fatalf("%s status URL = %q, want %q", dir, got[dir], wantURL)
		}
	}
}

// TestSameEndpointIgnoresLoopbackSpelling: the extension accepts localhost and
// 127.0.0.1 interchangeably, so comparing the strings would call a working
// machine misconfigured.
func TestSameEndpointIgnoresLoopbackSpelling(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"http://127.0.0.1:17311/status", "http://localhost:17311/status", true},
		{"http://localhost:17311/status", "http://localhost:17311/status", true},
		{"http://127.0.0.1:17311/status", "http://127.0.0.1:19311/status", false},
		{"http://127.0.0.1:17311/status", "", false},
		{"", "", false},
		{"not a url", "http://127.0.0.1:17311/status", false},
	}
	for _, tc := range tests {
		if got := sameEndpoint(tc.left, tc.right); got != tc.want {
			t.Fatalf("sameEndpoint(%q, %q) = %v, want %v", tc.left, tc.right, got, tc.want)
		}
	}
}

// TestDoctorOnlyContactsALoopbackStatusURL: both endpoints this check reads come
// from outside the process. A handshake report is written by whatever opened the
// bridge's unauthenticated websocket, and bridge-defaults.json is a file no
// release ever rewrites. doctor GETs the endpoint and prints it, so without a
// gate one forged handshake picks a hostname the operator's machine resolves and
// a URL it fetches. The counter is the assertion: every refused shape must cost
// zero requests, not merely a failed one.
func TestDoctorOnlyContactsALoopbackStatusURL(t *testing.T) {
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte(`{"connected":true}`))
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")

	tests := []struct {
		name string
		raw  string
		// want is the URL that may be fetched; "" means the endpoint is refused.
		want string
	}{
		{name: "the extension's own status URL", raw: "http://" + host + "/status", want: "http://" + host + "/status"},
		{name: "localhost is the same daemon", raw: "http://localhost:17311/status", want: "http://localhost:17311/status"},
		{name: "a bare loopback root gets the status path", raw: "http://127.0.0.1:17311", want: "http://127.0.0.1:17311/status"},
		{name: "IPv6 loopback", raw: "http://[::1]:17311/status", want: "http://[::1]:17311/status"},
		{name: "a query is dropped rather than fetched", raw: "http://127.0.0.1:17311/status?token=fixture", want: "http://127.0.0.1:17311/status"},
		{name: "another path on this very server is refused", raw: "http://" + host + "/exfil"},
		{name: "a percent-encoded spelling of the path is refused", raw: "http://" + host + "/%73tatus"},
		{name: "a trailing slash is a different path", raw: "http://" + host + "/status/"},
		{name: "a path parameter is refused", raw: "http://" + host + "/status;x=1"},
		{name: "a traversal out of the status path is refused", raw: "http://" + host + "/status/../../exfil"},
		{name: "a public host is refused", raw: "http://brw-doctor-must-not-resolve.invalid:80/status"},
		{name: "a hostname that merely contains a loopback address is refused", raw: "http://127.0.0.1.brw-doctor-must-not-resolve.invalid:80/status"},
		{name: "a name that resolves to loopback is still not an address", raw: "http://localtest.brw-doctor-must-not-resolve.invalid:80/status"},
		{name: "userinfo cannot smuggle the real host past the check", raw: "http://127.0.0.1:17311@brw-doctor-must-not-resolve.invalid:80/status"},
		{name: "https is refused", raw: "https://127.0.0.1:17311/status"},
		{name: "a non-http scheme is refused", raw: "file:///etc/passwd"},
		{name: "a missing port is refused", raw: "http://127.0.0.1/status"},
		{name: "the cloud metadata address is refused", raw: "http://169.254.169.254:80/status"},
		{name: "empty is refused", raw: ""},
	}

	client := &http.Client{Timeout: 2 * time.Second}
	var wantHits int64
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := loopbackStatusURL(tc.raw)
			if ok != (tc.want != "") {
				t.Fatalf("loopbackStatusURL(%q) ok = %v, want %v", tc.raw, ok, tc.want != "")
			}
			if got != tc.want {
				t.Fatalf("loopbackStatusURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// probeStatusURL is the only egress, so the gate has to hold there
			// too: a later caller must not be able to reach the network with an
			// endpoint it was handed. Probing is limited to the endpoints aimed
			// at this test server, so the counter below is exact.
			if tc.want != "" && !strings.Contains(tc.raw, host) {
				return
			}
			_, err := probeStatusURL(client, tc.raw)
			if tc.want == "" && err == nil {
				t.Fatalf("probeStatusURL(%q) contacted a refused endpoint", tc.raw)
			}
			if tc.want != "" && err != nil {
				t.Fatalf("probeStatusURL(%q) refused this profile's own bridge: %v", tc.raw, err)
			}
		})
		if tc.want != "" && strings.Contains(tc.raw, host) {
			wantHits++
		}
	}
	if got := atomic.LoadInt64(&hits); got != wantHits {
		t.Fatalf("the test server saw %d requests, want %d: an endpoint outside the loopback set was contacted", got, wantHits)
	}
}

// TestDoctorNeverEchoesAnEndpointItRefused: the string the check prints is the
// one an operator reads in a terminal, and a refused handshake is written by
// whoever opened the socket. Naming the fault without repeating the value is
// what keeps the report from becoming the attacker's output channel.
func TestDoctorNeverEchoesAnEndpointItRefused(t *testing.T) {
	const forged = "http://brw-doctor-must-not-resolve.invalid/exfil?token=fixture-not-a-token"

	tests := []struct {
		name         string
		reported     string
		packagedFile string
		wantDetails  []string
	}{
		{
			name:        "a forged refused handshake",
			reported:    forged,
			wantDetails: []string{"not http:// on a loopback port", "refused handshake"},
		},
		{
			name:         "an installed file naming somewhere else",
			packagedFile: `{"statusUrl":"` + forged + `"}`,
			wantDetails:  []string{"not http:// on a loopback port", setup.BridgeDefaultsFile},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t)
			if tc.packagedFile != "" {
				fx.writeFile(filepath.Join(fx.appDir, "extension", setup.BridgeDefaultsFile), tc.packagedFile)
			}
			fx.status.Connected = false
			fx.status.LastHandshake.StatusURL = tc.reported
			fx.status.LastHandshake.ConfigSource = "stored"

			check := checkByName(t, fx.report(), "bridge_config")
			if check.Status != checkFail {
				t.Fatalf("bridge_config = %s (%s), want fail", check.Status, check.Detail)
			}
			for _, want := range tc.wantDetails {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("bridge_config detail %q does not mention %q", check.Detail, want)
				}
			}
			for _, leaked := range []string{"brw-doctor-must-not-resolve", "exfil", "fixture-not-a-token"} {
				if strings.Contains(check.Detail, leaked) {
					t.Fatalf("bridge_config echoed a refused endpoint: %s", check.Detail)
				}
			}
			if check.Fix == "" {
				t.Fatal("a red bridge_config check printed no command to run")
			}
		})
	}
}

// TestDoctorNamesAnUnreadableAppDir: InstalledBridgeDefaults returns the same
// empty list for "no file anywhere" as for "this directory cannot be read", and
// the first of those is a near-clean verdict. A dropped error makes the two
// indistinguishable on the one input this check falls back to.
func TestDoctorNamesAnUnreadableAppDir(t *testing.T) {
	fx := newDoctorFixture(t)
	if err := os.Chmod(fx.appDir, 0o000); err != nil {
		t.Skipf("cannot make the app dir unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(fx.appDir, 0o755) })
	if _, err := os.ReadDir(fx.appDir); err == nil {
		t.Skip("this filesystem or uid ignores directory permissions")
	}

	check := checkByName(t, fx.report(), "bridge_config")
	if check.Status != checkWarn {
		t.Fatalf("bridge_config = %s (%s), want warn", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, fx.appDir) {
		t.Fatalf("bridge_config detail %q does not name the directory it could not read", check.Detail)
	}
}

// TestDoctorFollowsNoRedirectAwayFromAGatedEndpoint: gating the URL gates one
// request, and a redirect is the second. The actor the gate was built against —
// a local process that forges a handshake on the unauthenticated websocket —
// also binds a loopback port, so it can report an endpoint that PASSES the gate
// and then answer doctor's GET with a 302 to anywhere. Go's default client
// follows ten hops without re-gating, which hands back both halves of what the
// gate denies: the operator's machine contacts a host the reporter chose, and
// the reporter's string lands in the terminal through the url.Error of the hop
// that actually failed.
//
// The counter is the assertion. A failed second request is still a second
// request.
func TestDoctorFollowsNoRedirectAwayFromAGatedEndpoint(t *testing.T) {
	var reached int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reached, 1)
		_, _ = w.Write([]byte(`{"connected":true}`))
	}))
	defer target.Close()
	// Loopback, so the hop would succeed if it were taken: the test must not be
	// able to pass merely because a name failed to resolve.
	exfil := target.URL + "/exfil?token=fixture-not-a-token"

	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, exfil, http.StatusFound)
	}))
	defer hop.Close()
	hopStatus := hop.URL + "/status"

	t.Run("probeStatusURL makes exactly one request", func(t *testing.T) {
		client := &http.Client{Timeout: 2 * time.Second}
		_, err := probeStatusURL(client, hopStatus)
		if err == nil {
			t.Fatal("probeStatusURL accepted a redirect as a brw bridge answering")
		}
		for _, leaked := range []string{"exfil", "fixture-not-a-token"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("the probe error carries the redirect target: %v", err)
			}
		}
		if got := atomic.LoadInt64(&reached); got != 0 {
			t.Fatalf("the redirect target saw %d request(s); the gate was routed around by a 302", got)
		}
	})

	t.Run("the report says so without repeating where it was sent", func(t *testing.T) {
		fx := newDoctorFixture(t)
		fx.status.Connected = false
		fx.status.LastHandshake.StatusURL = hopStatus
		fx.status.LastHandshake.ConfigSource = "stored"

		check := checkByName(t, fx.report(), "bridge_config")
		if check.Status != checkFail {
			t.Fatalf("bridge_config = %s (%s), want fail", check.Status, check.Detail)
		}
		if !strings.Contains(check.Detail, "something other than a brw bridge answered") {
			t.Fatalf("bridge_config detail %q does not say what answered", check.Detail)
		}
		for _, leaked := range []string{"exfil", "fixture-not-a-token"} {
			if strings.Contains(check.Detail, leaked) {
				t.Fatalf("bridge_config echoed the redirect target: %s", check.Detail)
			}
		}
		if got := atomic.LoadInt64(&reached); got != 0 {
			t.Fatalf("the redirect target saw %d request(s) during a doctor run", got)
		}
	})
}

// TestDoctorNeverPrintsAFailureAboutAnEndpointItDidNotContact: a *url.Error
// names the URL whose request failed, which is only the gated one while nothing
// moved the request. Anything that does move it — a redirect today, a transport
// a later change installs — makes that URL a string the reporter chose, and
// endpointFailureDetail is where it would reach a terminal.
func TestDoctorNeverPrintsAFailureAboutAnEndpointItDidNotContact(t *testing.T) {
	const contacted = "http://127.0.0.1:17311/status"
	elsewhere := &url.Error{
		Op:  "Get",
		URL: "http://brw-doctor-must-not-resolve.invalid/exfil?token=fixture-not-a-token",
		Err: errors.New("dial tcp: lookup brw-doctor-must-not-resolve.invalid: no such host"),
	}

	tests := []struct {
		name string
		err  error
	}{
		{name: "an unclassified transport failure", err: elsewhere},
		{name: "an unexpected response", err: fmt.Errorf("%w: HTTP 302 from %s", errUnexpectedResponse, elsewhere.URL)},
		{
			// A failing URL that carries no scheme, so nothing in the message is
			// URL-shaped. The request it names is still not the one that was
			// gated, and that is the fact being checked.
			name: "a failure whose endpoint does not look like a URL",
			err: &url.Error{
				Op:  "Get",
				URL: "127.0.0.1:9/exfil",
				Err: errors.New("dial tcp: connect: connection refused"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detail := endpointFailureDetail(tc.err, contacted)
			for _, leaked := range []string{"brw-doctor-must-not-resolve", "exfil", "fixture-not-a-token", "127.0.0.1:9"} {
				if strings.Contains(detail, leaked) {
					t.Fatalf("endpointFailureDetail echoed an endpoint doctor never contacted: %s", detail)
				}
			}
		})
	}

	// The gated URL is still allowed to be described, or the check stops being
	// able to say anything useful about the endpoint it did contact.
	own := &url.Error{Op: "Get", URL: contacted, Err: errors.New("dial tcp: i/o timeout")}
	if detail := endpointFailureDetail(own, contacted); !strings.Contains(detail, "i/o timeout") {
		t.Fatalf("a failure about the gated endpoint was withheld: %s", detail)
	}
}

// TestEndpointFailureDetailClassifiesEveryProbeFailure enumerates the ways
// probeStatusURL can fail against a real listener and requires each to land on a
// branch that names a fix. The default branch is the one that pastes a raw
// transport error into the report, so a failure shape nobody classified is
// exactly the shape this check should not be printing verbatim: a new one
// arriving unclassified fails here rather than in a terminal.
func TestEndpointFailureDetailClassifiesEveryProbeFailure(t *testing.T) {
	hold := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-time.After(5 * time.Second):
		}
	}))
	defer silent.Close()
	defer close(hold)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer broken.Close()

	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>a captive portal</html>"))
	}))
	defer notJSON.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer redirecting.Close()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "an endpoint the gate refuses",
			url:  "http://brw-doctor-must-not-resolve.invalid:80/status",
			want: "it is not http:// on a loopback port, so it was not contacted",
		},
		{
			name: "a loopback port nothing bound",
			url:  "http://" + freeLoopbackAddr(t) + "/status",
			want: "nothing is listening there",
		},
		{
			name: "a listener that never answers",
			url:  silent.URL + "/status",
			want: "the connection was accepted but /status did not answer in time",
		},
		{
			name: "a listener that is not a brw bridge",
			url:  broken.URL + "/status",
			want: "something other than a brw bridge answered",
		},
		{
			name: "a listener that answers with something other than JSON",
			url:  notJSON.URL + "/status",
			want: "something other than a brw bridge answered",
		},
		{
			name: "a listener that redirects",
			url:  redirecting.URL + "/status",
			want: "something other than a brw bridge answered",
		},
	}

	// Long enough that a loaded machine cannot turn a local round trip into the
	// timeout case, short enough that the one listener that never answers costs
	// a second and a half.
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gated, _ := loopbackStatusURL(tc.url)
			_, err := probeStatusURL(client, tc.url)
			if err == nil {
				t.Fatalf("probeStatusURL(%q) reported a healthy bridge", tc.url)
			}
			detail := endpointFailureDetail(err, gated)
			if !strings.Contains(detail, tc.want) {
				t.Fatalf("endpointFailureDetail(%v) = %q, want it to say %q", err, detail, tc.want)
			}
			if strings.HasPrefix(detail, "it is unreachable") {
				t.Fatalf("%q fell through to the unclassified branch: %s", tc.url, detail)
			}
		})
	}
}

// TestDoctorBridgeConfigDoesNotGoRedOverAnOverriddenFile: an installed
// bridge-defaults.json loses to the extension's stored config, so a machine
// whose extension is connected and working is working whatever that file says —
// including when it names an endpoint this check may not contact. Deciding on
// the file before reading the live report turns a working machine red, which is
// the failure mode the whole check exists to avoid, just from the other side.
func TestDoctorBridgeConfigDoesNotGoRedOverAnOverriddenFile(t *testing.T) {
	const forged = "http://brw-doctor-must-not-resolve.invalid/exfil?token=fixture-not-a-token"
	fx := newDoctorFixture(t)
	fx.writeFile(filepath.Join(fx.appDir, "extension", setup.BridgeDefaultsFile),
		`{"statusUrl":"`+forged+`"}`)
	fx.status.Connected = true
	fx.status.Hello.StatusURL = "http://" + strings.TrimPrefix(fx.bridge.URL, "http://") + "/status"
	fx.status.Hello.ConfigSource = "stored"

	report := fx.report()
	check := checkByName(t, report, "bridge_config")
	if check.Status != checkOK {
		t.Fatalf("bridge_config = %s (%s), want ok: a connected extension overrides the file", check.Status, check.Detail)
	}
	for _, want := range []string{"the extension is using", setup.BridgeDefaultsFile, "not http:// on a loopback port", "overridden"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("bridge_config detail %q does not mention %q", check.Detail, want)
		}
	}
	for _, leaked := range []string{"brw-doctor-must-not-resolve", "exfil", "fixture-not-a-token"} {
		if strings.Contains(check.Detail, leaked) {
			t.Fatalf("bridge_config echoed the file's endpoint: %s", check.Detail)
		}
	}
	if !report.OK {
		t.Fatalf("a working machine reported failures: %v", report.Failures)
	}
}

// TestDoctorBridgeConfigSkipNamesACheckThatIsActuallyRed covers the third drift
// shape: a stored bridgeUrl or bridgePort moves the websocket URL too, so no
// handshake ever reaches this daemon to be refused and there is nothing here to
// name. The skip is the honest answer, and it is only honest while the check it
// sends the operator to is the one going red on the same machine — otherwise it
// is a green-looking report on a machine that cannot drive a browser.
//
// The assertion is the report, not the wording: every check the skip detail
// names has to be red in that same report.
func TestDoctorBridgeConfigSkipNamesACheckThatIsActuallyRed(t *testing.T) {
	fx := newDoctorFixture(t)
	// What the shape looks like from here: the extension is talking to some other
	// websocket URL, so this daemon has no connection, no hello and no refusal,
	// and no file on disk names anything either.
	fx.status = bridgeStatus{}

	report := fx.report()
	skip := checkByName(t, report, "bridge_config")
	if skip.Status != checkSkip {
		t.Fatalf("bridge_config = %s (%s), want skip", skip.Status, skip.Detail)
	}
	named := 0
	for _, other := range report.Checks {
		if other.Name == "bridge_config" || !strings.Contains(skip.Detail, other.Name) {
			continue
		}
		named++
		if other.Status != checkFail {
			t.Fatalf("bridge_config skipped and sent the operator to %s, which is %s: %s",
				other.Name, other.Status, other.Detail)
		}
	}
	if named == 0 {
		t.Fatalf("bridge_config skipped without naming a check that does go red: %s", skip.Detail)
	}
}
