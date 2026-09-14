package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// environmentFixture is a plain page. Every environment assertion below is made
// from inside it, so what is being checked is what the PAGE and the SERVER see,
// not what brw sent.
const environmentFixture = `<html><head><title>environment</title></head><body><h1>environment</h1></body></html>`

func serveEnvironmentFixture(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, environmentFixture)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// evaluateString runs an expression that resolves to a string.
func evaluateString(t *testing.T, m *Manager, ctx context.Context, expr string) string {
	t.Helper()
	value, err := m.Evaluate(ctx, expr)
	if err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	text, _ := value.(string)
	return text
}

func evaluateBool(t *testing.T, m *Manager, ctx context.Context, expr string) bool {
	t.Helper()
	value, err := m.Evaluate(ctx, expr)
	if err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	got, _ := value.(bool)
	return got
}

// The override is only real if the PAGE sees it: a caller testing
// location-gated behaviour needs navigator.geolocation to answer with the
// injected position, not merely for CDP to have accepted the command.
func TestSetGeolocationIsWhatNavigatorGeolocationReports(t *testing.T) {
	tests := []struct {
		name          string
		lat, lng, acc float64
	}{
		{"a city", 51.5007, -0.1246, 25},
		// Zero is the coordinate a struct tag can silently drop, and CDP reads a
		// missing coordinate as "position unavailable" rather than as zero.
		{"the prime meridian", 51.4779, 0, 10},
		{"null island", 0, 0, 5},
	}

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emulationTab(t, m, ctx, serveEnvironmentFixture(t))

			lat, lng, acc := tt.lat, tt.lng, tt.acc
			result, err := m.SetGeolocation(ctx, GeolocationOptions{Latitude: &lat, Longitude: &lng, Accuracy: &acc})
			if err != nil {
				t.Fatalf("SetGeolocation: %v", err)
			}
			if result.Geolocation == nil || result.Geolocation.Latitude != tt.lat {
				t.Fatalf("result geolocation = %+v, want latitude %v", result.Geolocation, tt.lat)
			}

			reported := evaluateString(t, m, ctx, `new Promise(resolve => navigator.geolocation.getCurrentPosition(
				p => resolve(p.coords.latitude + "," + p.coords.longitude + "," + p.coords.accuracy),
				e => resolve("error:" + e.code + ":" + e.message),
				{timeout: 10000}))`)
			want := fmt.Sprintf("%v,%v,%v", tt.lat, tt.lng, tt.acc)
			if reported != want {
				t.Fatalf("navigator.geolocation reported %q, want %q", reported, want)
			}

			if _, err := m.SetGeolocation(ctx, GeolocationOptions{Clear: true}); err != nil {
				t.Fatalf("clear geolocation: %v", err)
			}
		})
	}
}

// Offline has to fail a request the page makes, not just set a flag: an app's
// offline path is entered by a failed fetch.
func TestSetNetworkConditionsOfflineFailsAFetch(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url := serveEnvironmentFixture(t)
	emulationTab(t, m, ctx, url)

	probe := fmt.Sprintf(`fetch(%q + "?probe=1", {cache: "no-store"}).then(r => "status:" + r.status).catch(e => "failed:" + e.name)`, url)
	if got := evaluateString(t, m, ctx, probe); !strings.HasPrefix(got, "status:200") {
		t.Fatalf("fetch before going offline = %q, want status:200 — the fixture has to be reachable first for the offline assertion to mean anything", got)
	}

	offline := true
	result, err := m.SetNetworkConditions(ctx, NetworkConditionsOptions{Offline: &offline})
	if err != nil {
		t.Fatalf("SetNetworkConditions: %v", err)
	}
	if result.Network == nil || !result.Network.Offline {
		t.Fatalf("result network = %+v, want offline", result.Network)
	}

	if got := evaluateString(t, m, ctx, probe); !strings.HasPrefix(got, "failed:") {
		t.Fatalf("fetch while offline = %q, want it to fail", got)
	}
	if online := evaluateBool(t, m, ctx, `navigator.onLine`); online {
		t.Error("navigator.onLine is still true while the tab is emulated offline")
	}

	if _, err := m.SetNetworkConditions(ctx, NetworkConditionsOptions{Clear: true}); err != nil {
		t.Fatalf("clear network conditions: %v", err)
	}
	if got := evaluateString(t, m, ctx, probe); !strings.HasPrefix(got, "status:200") {
		t.Fatalf("fetch after clearing = %q, want status:200", got)
	}
}

// What matters is the media query result the page's own CSS is evaluated
// against, which is what decides whether it renders its dark theme.
func TestEmulateMediaFlipsPrefersColorScheme(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveEnvironmentFixture(t))

	tests := []struct {
		name     string
		scheme   string
		darkWant bool
	}{
		{"dark", "dark", true},
		{"light", "light", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.EmulateMedia(ctx, MediaEmulationOptions{ColorScheme: tt.scheme}); err != nil {
				t.Fatalf("EmulateMedia: %v", err)
			}
			if got := evaluateBool(t, m, ctx, `matchMedia('(prefers-color-scheme: dark)').matches`); got != tt.darkWant {
				t.Fatalf("prefers-color-scheme: dark matched %v under color_scheme %q, want %v", got, tt.scheme, tt.darkWant)
			}
		})
	}

	if _, err := m.EmulateMedia(ctx, MediaEmulationOptions{Media: "print", ReducedMotion: "reduce"}); err != nil {
		t.Fatalf("EmulateMedia print: %v", err)
	}
	if !evaluateBool(t, m, ctx, `matchMedia('print').matches`) {
		t.Error("print media query does not match under media:print")
	}
	if !evaluateBool(t, m, ctx, `matchMedia('(prefers-reduced-motion: reduce)').matches`) {
		t.Error("prefers-reduced-motion: reduce does not match under reduced_motion:reduce")
	}

	if _, err := m.EmulateMedia(ctx, MediaEmulationOptions{Clear: true}); err != nil {
		t.Fatalf("clear emulated media: %v", err)
	}
	if evaluateBool(t, m, ctx, `matchMedia('print').matches`) {
		t.Error("print media query still matches after clearing the override")
	}
}

// headerRecorder collects what a fixture server actually received, which is the
// only place the scoping question can be answered.
type headerRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (h *headerRecorder) record(value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, value)
}

func (h *headerRecorder) values() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

const scopedHeaderName = "X-Brw-Scoped"

func serveHeaderRecorder(t *testing.T, recorder *headerRecorder) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r.Header.Get(scopedHeaderName))
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, environmentFixture)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A header set the browser-wide way rides on every request the page makes, so a
// bearer token declared for one API also reaches the page's analytics beacons
// and font CDNs. The declared origin has to be the only one that sees it.
func TestExtraHeadersReachOnlyTheDeclaredOrigin(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var declaredSeen, otherSeen headerRecorder
	declaredURL := serveHeaderRecorder(t, &declaredSeen)
	otherURL := serveHeaderRecorder(t, &otherSeen)

	emulationTab(t, m, ctx, declaredURL)

	result, err := m.SetExtraHeaders(ctx, ExtraHeadersOptions{Origins: []OriginHeaders{{
		Origin:  declaredURL,
		Headers: map[string]string{scopedHeaderName: "declared-origin-only"},
	}}})
	if err != nil {
		t.Fatalf("SetExtraHeaders: %v", err)
	}
	if len(result.ExtraHeaders) != 1 || len(result.ExtraHeaders[0].Headers) != 1 {
		t.Fatalf("result extra headers = %+v, want one origin with one header name", result.ExtraHeaders)
	}
	// The value is a credential in the common case; a result that echoed it would
	// put it in the agent transcript and the client's logs.
	for _, echoed := range result.ExtraHeaders[0].Headers {
		if echoed == "declared-origin-only" {
			t.Fatal("the result echoed the header VALUE; only names may be echoed")
		}
	}

	fetchFrom := func(target string) string {
		return evaluateString(t, m, ctx,
			fmt.Sprintf(`fetch(%q + "?probe=1", {cache: "no-store"}).then(r => "status:" + r.status).catch(e => "failed:" + e.name)`, target))
	}
	if got := fetchFrom(declaredURL); !strings.HasPrefix(got, "status:200") {
		t.Fatalf("fetch to the declared origin = %q, want status:200", got)
	}
	if got := fetchFrom(otherURL); !strings.HasPrefix(got, "status:200") {
		t.Fatalf("fetch to the other origin = %q, want status:200", got)
	}

	if !containsValue(declaredSeen.values(), "declared-origin-only") {
		t.Fatalf("the declared origin never received the header; it saw %q", declaredSeen.values())
	}
	for _, value := range otherSeen.values() {
		if value != "" {
			t.Fatalf("an undeclared origin received the header %q; per-origin headers must not be blanket-applied", value)
		}
	}

	if _, err := m.SetExtraHeaders(ctx, ExtraHeadersOptions{Clear: true}); err != nil {
		t.Fatalf("clear extra headers: %v", err)
	}
	before := len(declaredSeen.values())
	if got := fetchFrom(declaredURL); !strings.HasPrefix(got, "status:200") {
		t.Fatalf("fetch after clearing = %q, want status:200", got)
	}
	after := declaredSeen.values()
	if len(after) <= before {
		t.Fatal("the post-clear fetch never reached the fixture")
	}
	if after[len(after)-1] != "" {
		t.Fatalf("the header %q was still attached after clearing", after[len(after)-1])
	}
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A user-agent override that only changes navigator.userAgent is a half
// override: server-side device detection reads the request header.
func TestSetUserAgentChangesWhatTheServerSees(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.UserAgent())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, environmentFixture)
	}))
	t.Cleanup(srv.Close)

	emulationTab(t, m, ctx, srv.URL)
	original := evaluateString(t, m, ctx, `navigator.userAgent`)

	const override = "BrwEnvironmentTest/1.0 (fabricated; not a real browser)"
	result, err := m.SetUserAgent(ctx, UserAgentOptions{
		UserAgent:      override,
		AcceptLanguage: "fr-FR,fr;q=0.9",
		Platform:       "FabricatedOS",
	})
	if err != nil {
		t.Fatalf("SetUserAgent: %v", err)
	}
	if result.UserAgent == nil || result.UserAgent.UserAgent != override {
		t.Fatalf("result user agent = %+v, want %q", result.UserAgent, override)
	}

	if got := evaluateString(t, m, ctx, `navigator.userAgent`); got != override {
		t.Fatalf("navigator.userAgent = %q, want %q", got, override)
	}
	if got := evaluateString(t, m, ctx, `navigator.platform`); got != "FabricatedOS" {
		t.Fatalf("navigator.platform = %q, want %q", got, "FabricatedOS")
	}
	if got := evaluateString(t, m, ctx, `navigator.language`); got != "fr-FR" {
		t.Errorf("navigator.language = %q, want fr-FR", got)
	}

	if _, err := m.NavigateTo(ctx, srv.URL); err != nil {
		t.Fatalf("reload under the override: %v", err)
	}
	mu.Lock()
	lastSeen := seen[len(seen)-1]
	mu.Unlock()
	if lastSeen != override {
		t.Fatalf("the server saw User-Agent %q, want %q — the override never reached the request header", lastSeen, override)
	}

	if _, err := m.SetUserAgent(ctx, UserAgentOptions{Clear: true}); err != nil {
		t.Fatalf("clear user agent: %v", err)
	}
	if got := evaluateString(t, m, ctx, `navigator.userAgent`); got != original {
		t.Fatalf("user agent after clearing = %q, want the captured original %q", got, original)
	}
}

// Credentials are supplied per call and dropped after use, so the daemon must
// hold nothing once the call returns.
func TestAuthenticateAnswersTheChallengeAndRetainsNothing(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		user              = "fixture-user"
		fixtureCredential = "fixture-basic-auth-value-one"
		body              = "authenticated-fixture-content"
		// Carried in the credentialed URL's query string, which is exactly what a
		// replayable trace must not keep.
		urlMarker = "fixture-url-marker-3d5a"
	)
	var challenges int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPass != fixtureCredential {
			mu.Lock()
			challenges++
			mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p id="content">%s</p></body></html>`, body)
	}))
	t.Cleanup(srv.Close)

	emulationTab(t, m, ctx, "about:blank")

	credentialedURL := srv.URL + "/protected?grant=" + urlMarker
	// Dropped so the only navigate_to entry left is the credentialed one; opening
	// the tab above made an ordinary, deliberately unredacted one.
	m.ClearTrace()
	// The MCP and HTTP layers both mark this call sensitive; the trace redaction
	// under test is what that mark is for, so the test supplies it too.
	result, err := m.Authenticate(WithSensitiveAction(ctx), CredentialsOptions{
		Origin:   srv.URL,
		Username: user,
		Password: fixtureCredential,
		URL:      credentialedURL,
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if result.Authenticated == nil || !result.Authenticated.Challenged {
		t.Fatalf("authentication outcome = %+v, want a recorded challenge", result.Authenticated)
	}
	if result.Authenticated.Answered == 0 {
		t.Fatal("no challenge was answered, so the credentials never reached the server")
	}
	if !result.Authenticated.BrowserCached {
		t.Fatal("browser_cached is false after an answered challenge; the browser keeps the credential and the result has to say so")
	}

	// The page loaded the protected body, which only a correctly answered
	// challenge produces.
	if got := evaluateString(t, m, ctx, `document.getElementById("content") ? document.getElementById("content").textContent : "unauthenticated"`); got != body {
		t.Fatalf("page content = %q, want %q", got, body)
	}
	mu.Lock()
	seenChallenges := challenges
	mu.Unlock()
	if seenChallenges == 0 {
		t.Fatal("the fixture never issued a 401, so this proves nothing about answering one")
	}

	// Nothing retains the credentials. The environment state is where an armed
	// credential lives, so scan it for the password rather than asserting on a
	// count that a future field could make meaningless.
	if held := holdsSecret(reflect.ValueOf(&m.env).Elem(), fixtureCredential, map[uintptr]bool{}); held {
		t.Fatal("the password is still reachable from the manager's environment state after the call returned")
	}
	if m.env.authArmed(m.refs.Active()) {
		t.Fatal("authentication handling is still armed for the tab after the call returned")
	}
	// The trace is replayable and long-lived, and a credentialed URL is the kind
	// that carries a token in its query string.
	var sawNavigation bool
	for _, entry := range m.GetTrace().Entries {
		if strings.Contains(entry.Text, fixtureCredential) || strings.Contains(entry.Value, fixtureCredential) {
			t.Fatal("the password was recorded in the replayable action trace")
		}
		if strings.Contains(entry.Text, urlMarker) || strings.Contains(entry.Value, urlMarker) {
			t.Fatalf("the credentialed URL's query string reached the trace as %q; a sensitive navigation must record that it happened, not where it went", entry.Text)
		}
		if entry.Action == "navigate_to" {
			sawNavigation = true
			if !entry.Redacted {
				t.Fatalf("the credentialed navigation is in the trace unredacted as %q", entry.Text)
			}
		}
	}
	if !sawNavigation {
		t.Fatal("no navigate_to entry reached the trace, so the redaction assertion above proves nothing")
	}
}

// brw drops its own copy of the credential, but Chrome keeps one: an answered
// challenge goes into the browser's HTTP-auth cache for the rest of the session
// and no CDP command empties it. The tool description, the docs and the result
// all say so now, so the behaviour they describe is pinned here.
func TestTheBrowserKeepsTheCredentialAfterAuthenticateReturns(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		user              = "fixture-user"
		fixtureCredential = "fixture-basic-auth-value-two"
		body              = "authenticated-fixture-content"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPass != fixtureCredential {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><p id="content">%s</p></body></html>`, body)
	}))
	t.Cleanup(srv.Close)

	emulationTab(t, m, ctx, "about:blank")

	if _, err := m.Authenticate(ctx, CredentialsOptions{
		Origin:   srv.URL,
		Username: user,
		Password: fixtureCredential,
		URL:      srv.URL + "/protected",
	}); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// Nothing is armed now: dropCredential ran and the deferred sync turned
	// interception off. A plain navigation is the same one an agent or a human
	// would make next.
	if m.env.authArmed(m.refs.Active()) {
		t.Fatal("a credential is still armed, so the next navigation proves nothing about the browser's own cache")
	}
	if _, err := m.NavigateTo(ctx, srv.URL+"/another-protected-page"); err != nil {
		t.Fatalf("navigate to a second protected path: %v", err)
	}
	content := evaluateString(t, m, ctx, `document.getElementById("content") ? document.getElementById("content").textContent : "unauthenticated"`)
	if content != body {
		t.Fatalf("a second protected path loaded %q rather than the protected body; if the browser has stopped caching the credential, brw_authenticate's description, docs/install.md and AuthenticationOutcome.BrowserCached all need correcting the other way", content)
	}
}

// Chrome has no getPermission, so brw remembers what it found. Resetting to
// prompt regardless would revoke a grant the human made, on a persistent profile
// they keep using, as a side effect of clearing an override brw installed.
func TestClearingGeolocationRestoresThePermissionBrwFound(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, serveEnvironmentFixture(t))
	permissionState := func() string {
		return evaluateString(t, m, ctx, `navigator.permissions.query({name:"geolocation"}).then(status => status.state)`)
	}
	lat, lng := 51.5007, -0.1246

	if got := permissionState(); got != "prompt" {
		t.Fatalf("a fresh origin starts at %q, want prompt; the rest of this test reads from that baseline", got)
	}
	if _, err := m.SetGeolocation(ctx, GeolocationOptions{Latitude: &lat, Longitude: &lng}); err != nil {
		t.Fatalf("SetGeolocation: %v", err)
	}
	if got := permissionState(); got != "granted" {
		t.Fatalf("permission = %q after an override, want granted — the override would be invisible to the page", got)
	}
	if _, err := m.SetGeolocation(ctx, GeolocationOptions{Clear: true}); err != nil {
		t.Fatalf("clear geolocation: %v", err)
	}
	if got := permissionState(); got != "prompt" {
		t.Fatalf("permission = %q after clearing an override brw granted, want prompt", got)
	}

	// Now the case that matters: the site already had geolocation before brw
	// touched it.
	origin := evaluateString(t, m, ctx, `location.origin`)
	if err := m.setGeolocationPermission(ctx, origin, cdpbrowser.PermissionSettingGranted); err != nil {
		t.Fatalf("grant geolocation the way a human would have: %v", err)
	}
	if got := permissionState(); got != "granted" {
		t.Fatalf("permission = %q after the standing grant, want granted", got)
	}
	if _, err := m.SetGeolocation(ctx, GeolocationOptions{Latitude: &lat, Longitude: &lng}); err != nil {
		t.Fatalf("SetGeolocation over a standing grant: %v", err)
	}
	if _, err := m.SetGeolocation(ctx, GeolocationOptions{Clear: true}); err != nil {
		t.Fatalf("clear geolocation over a standing grant: %v", err)
	}
	if got := permissionState(); got != "granted" {
		t.Fatalf("permission = %q after clearing, want the standing grant back; brw revoked a permission it did not grant", got)
	}
}

// Switching the download directory must not take away files that are already on
// disk: their recorded paths point inside the staging directory, and
// brw_downloads goes on reporting them.
func TestSetDownloadPathKeepsFilesDownloadedBeforeIt(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, "data:text/html,<html><body>download</body></html>")
	// Arms tracking and the managed staging directory, which is where this first
	// download has to land for the test to mean anything.
	if _, err := m.Downloads(ctx); err != nil {
		t.Fatalf("downloads: %v", err)
	}

	const staged = "staged-before-the-switch"
	triggerEnvironmentDownload(t, m, ctx, staged, "staged.txt")
	first := awaitCompletedDownload(t, m, ctx, "")
	m.downloadsMu.Lock()
	stagingDir := m.downloadDir
	m.downloadsMu.Unlock()
	if filepath.Dir(first.Path) != stagingDir {
		t.Fatalf("the first download landed at %q, want it inside the managed staging directory %q", first.Path, stagingDir)
	}

	target := filepath.Join(t.TempDir(), "brw-download-target")
	if _, err := m.SetDownloadPath(ctx, DownloadPathOptions{Path: target}); err != nil {
		t.Fatalf("SetDownloadPath: %v", err)
	}

	contents, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("the file downloaded before the switch was deleted by it, at the path brw_downloads still reports: %v", err)
	}
	if string(contents) != staged {
		t.Fatalf("the earlier download now reads %q, want %q", contents, staged)
	}
	snapshot, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("downloads after the switch: %v", err)
	}
	for _, entry := range snapshot.Downloads {
		if entry.GUID != first.GUID {
			continue
		}
		if entry.Path != first.Path {
			t.Fatalf("brw_downloads now reports %q for the earlier download, want the unchanged %q", entry.Path, first.Path)
		}
	}
}

// triggerEnvironmentDownload downloads a blob from the page, which is the
// cheapest way to make a real Chrome download without a fixture server.
func triggerEnvironmentDownload(t *testing.T, m *Manager, ctx context.Context, body, filename string) {
	t.Helper()
	trigger := fmt.Sprintf(`(function(){
		var blob = new Blob([%q], {type:"text/plain"});
		var a = document.createElement("a");
		a.href = URL.createObjectURL(blob);
		a.download = %q;
		document.body.appendChild(a);
		a.click();
		return true;
	})()`, body, filename)
	if _, err := m.Evaluate(ctx, trigger); err != nil {
		t.Fatalf("trigger download: %v", err)
	}
}

// awaitCompletedDownload waits for a completed entry other than skipGUID.
func awaitCompletedDownload(t *testing.T, m *Manager, ctx context.Context, skipGUID string) DownloadEntry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := m.Downloads(ctx)
		if err != nil {
			t.Fatalf("downloads: %v", err)
		}
		for _, entry := range snapshot.Downloads {
			if entry.State == string(downloadStateCompleted) && entry.GUID != skipGUID {
				return entry
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("no download completed within the deadline")
	return DownloadEntry{}
}

// holdsSecret walks a value looking for the secret in any reachable string. It
// is deliberately structural rather than a check of one field: the assertion is
// "the daemon does not retain this", and a field added later must not quietly
// opt out of it.
func holdsSecret(v reflect.Value, secret string, visited map[uintptr]bool) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), secret)
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
		if v.Kind() == reflect.Pointer {
			if visited[v.Pointer()] {
				return false
			}
			visited[v.Pointer()] = true
		}
		return holdsSecret(v.Elem(), secret, visited)
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if holdsSecret(key, secret, visited) || holdsSecret(v.MapIndex(key), secret, visited) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if holdsSecret(v.Index(i), secret, visited) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			// Unexported fields are exactly where a credential would sit, so read
			// them through an addressable alias rather than skipping them.
			if !field.CanInterface() && field.CanAddr() {
				field = reflect.NewAt(field.Type(), field.Addr().UnsafePointer()).Elem()
			}
			if holdsSecret(field, secret, visited) {
				return true
			}
		}
	}
	return false
}

// A download path is only useful if the file is actually there and brw_downloads
// reports the path it landed at.
func TestSetDownloadPathPutsTheFileWhereTheCallerAsked(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, "data:text/html,<html><body>download</body></html>")

	dir := filepath.Join(t.TempDir(), "brw-download-target")
	result, err := m.SetDownloadPath(ctx, DownloadPathOptions{Path: dir})
	if err != nil {
		t.Fatalf("SetDownloadPath: %v", err)
	}
	if result.DownloadPath != dir {
		t.Fatalf("download path = %q, want %q", result.DownloadPath, dir)
	}

	trigger := `(function(){
		var blob = new Blob(["environment-download-fixture"], {type:"text/plain"});
		var a = document.createElement("a");
		a.href = URL.createObjectURL(blob);
		a.download = "environment.txt";
		document.body.appendChild(a);
		a.click();
		return true;
	})()`
	if _, err := m.Evaluate(ctx, trigger); err != nil {
		t.Fatalf("trigger download: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var done DownloadEntry
	for time.Now().Before(deadline) {
		snapshot, err := m.Downloads(ctx)
		if err != nil {
			t.Fatalf("downloads: %v", err)
		}
		for _, entry := range snapshot.Downloads {
			if entry.State == string(downloadStateCompleted) {
				done = entry
			}
		}
		if done.GUID != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if done.GUID == "" {
		t.Fatal("no download completed within the deadline")
	}
	if filepath.Dir(done.Path) != dir {
		t.Fatalf("brw_downloads reported path %q, want it inside %q", done.Path, dir)
	}
	contents, err := os.ReadFile(done.Path)
	if err != nil {
		t.Fatalf("read the downloaded file at the reported path: %v", err)
	}
	if string(contents) != "environment-download-fixture" {
		t.Fatalf("downloaded contents = %q, want the fixture body", contents)
	}

	// Clearing must not delete a directory brw did not create.
	if _, err := m.SetDownloadPath(ctx, DownloadPathOptions{Clear: true}); err != nil {
		t.Fatalf("clear download path: %v", err)
	}
	if _, err := os.Stat(done.Path); err != nil {
		t.Fatalf("the caller's downloaded file was removed when the path was cleared: %v", err)
	}
}

// The armed credential is keyed by tab. Two Authenticate calls landing on one
// tab at once would overwrite each other's entry, and the first to finish would
// drop the second's while its navigation was still in flight, so each one's
// challenge has to be answered with its own password.
func TestTwoAuthenticateCallsOnOneTabDoNotClobberEachOther(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	credentials := map[string]struct{ user, password string }{
		"/first":  {"user-a", "fixture-pw-a-24d9"},
		"/second": {"user-b", "fixture-pw-b-7c03"},
	}
	var mu sync.Mutex
	authenticated := map[string]bool{}
	challengedFirst := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, known := credentials[r.URL.Path]
		gotUser, gotPass, ok := r.BasicAuth()
		if !known || !ok || gotUser != want.user || gotPass != want.password {
			if r.URL.Path == "/first" {
				// Held open so that an unserialised second call would have armed its
				// own credential before Chrome ever raises this challenge.
				once.Do(func() { close(challengedFirst) })
				time.Sleep(250 * time.Millisecond)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		authenticated[r.URL.Path] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, environmentFixture)
	}))
	t.Cleanup(srv.Close)

	emulationTab(t, m, ctx, "about:blank")

	authenticate := func(path string) error {
		want := credentials[path]
		_, err := m.Authenticate(ctx, CredentialsOptions{
			Origin:   srv.URL,
			Username: want.user,
			Password: want.password,
			URL:      srv.URL + path,
		})
		return err
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- authenticate("/first") }()

	select {
	case <-challengedFirst:
	case <-time.After(30 * time.Second):
		t.Fatal("the first call never reached the fixture")
	}
	secondErr := authenticate("/second")
	firstErr := <-firstDone
	if firstErr != nil {
		t.Fatalf("the first Authenticate failed: %v", firstErr)
	}
	if secondErr != nil {
		t.Fatalf("the second Authenticate failed: %v", secondErr)
	}

	mu.Lock()
	defer mu.Unlock()
	for path := range credentials {
		if !authenticated[path] {
			t.Fatalf("%s was never reached with its own credentials; a concurrent call on the same tab answered or dropped this one's", path)
		}
	}
}
