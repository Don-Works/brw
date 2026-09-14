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

	result, err := m.Authenticate(ctx, CredentialsOptions{
		Origin:   srv.URL,
		Username: user,
		Password: fixtureCredential,
		URL:      srv.URL + "/protected",
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
	for _, entry := range m.GetTrace().Entries {
		if strings.Contains(entry.Text, fixtureCredential) || strings.Contains(entry.Value, fixtureCredential) {
			t.Fatal("the password was recorded in the replayable action trace")
		}
	}
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
