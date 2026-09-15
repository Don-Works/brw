package recipe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

// What this file adds to compile_test.go: a real browser and a real session.
//
// The replay there drives a compiled recipe against a page model built from the
// recorded observations, which proves the compiler and the runner agree with
// each other and nothing about a browser. "Replays green twice on a fresh
// profile" is a claim about Chrome resolving the compiled semantic targets on a
// page it rendered itself, and about the second run starting signed out and
// signing itself in.
//
// A real third-party site would need live credentials and network, which this
// suite must never depend on. The fixture below is the achievable version: an
// httptest site with a login step, a session cookie, and pages that refuse a
// request that does not carry it.

const (
	fixtureSessionCookie = "brw_fixture_session"
	fixtureOperator      = "Signed in as fixture operator"
	fixtureSearchTerm    = "quarterly"
)

// signedInSite is the fixture. Its pages exist only for a request carrying the
// session cookie; anything else gets 401 and a page with none of the signed-in
// controls on it.
type signedInSite struct {
	mu sync.Mutex
	// sessions holds every token handed out, so a test can say whether the
	// second run signed in on its own rather than reusing the first run's.
	sessions []string
	// dropCookie makes the login response set a cookie that has already
	// expired. The browser discards it, so the session the site created is one
	// the browser never presents — which is what a dropped session cookie is.
	dropCookie bool
	// refusals counts requests to a protected page that carried no session.
	refusals int
	issued   int
}

func (s *signedInSite) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(fixtureSessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, token := range s.sessions {
		if token == cookie.Value {
			return true
		}
	}
	return false
}

func (s *signedInSite) refuse(w http.ResponseWriter) {
	s.mu.Lock()
	s.refusals++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusUnauthorized)
	// Deliberately not a copy of the signed-in page with the data blanked: the
	// refusal carries none of the controls the recipe acts on, so a replay that
	// lost its session cannot resolve a single step.
	fmt.Fprint(w, `<!doctype html><title>Sign in required</title><h1>Sign in required</h1>
<p>This page is only served to a signed-in session.</p>`)
}

func (s *signedInSite) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>Sign in</title>
<h1>Reports</h1>
<form method="POST" action="/session"><button type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.issued++
		token := fmt.Sprintf("fixture-session-%d", s.issued)
		s.sessions = append(s.sessions, token)
		dropped := s.dropCookie
		s.mu.Unlock()
		cookie := &http.Cookie{Name: fixtureSessionCookie, Value: token, Path: "/"}
		if dropped {
			// The site still issues the session; the browser is told the cookie
			// expired in 1970 and keeps nothing. Every later request arrives
			// without it.
			cookie.Expires = time.Unix(0, 0).UTC()
			cookie.MaxAge = -1
		}
		http.SetCookie(w, cookie)
		http.Redirect(w, r, "/reports", http.StatusSeeOther)
	})
	mux.HandleFunc("/reports", func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			s.refuse(w)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		results := ""
		if strings.Contains(strings.ToLower(r.URL.Query().Get("q")), fixtureSearchTerm) {
			results = `<ul><li><a href="/reports/quarterly-revenue">Quarterly revenue</a></li></ul>`
		}
		fmt.Fprintf(w, `<!doctype html><title>Reports</title>
<h1>%s</h1>
<form method="GET" action="/reports">
  <label for="q">Report name</label>
  <input id="q" name="q" type="text">
  <button type="submit">Search reports</button>
</form>
%s`, fixtureOperator, results)
	})
	mux.HandleFunc("/reports/quarterly-revenue", func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			s.refuse(w)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><title>Quarterly revenue</title>
<h1>%s</h1>
<h2>Quarterly revenue</h2>
<p role="status">Export ready</p>`, fixtureOperator)
	})
	return mux
}

func (s *signedInSite) counts() (issued, refusals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued, s.refusals
}

// liveChrome starts headless Chrome on a profile of its own. Every call is a
// fresh profile: a new user-data-dir is a new cookie jar, which is what makes
// the second replay sign itself in rather than inherit a session.
func liveChrome(t *testing.T) *browser.Manager {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	profile := browsertest.NewProfile(t)
	manager, err := browser.New(ctx, browser.Config{
		Headless:    true,
		UserDataDir: profile.Dir(),
		Timeout:     30 * time.Second,
	})
	if err != nil {
		cancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	profile.StopWith(func() {
		_ = manager.Close()
		cancel()
	})
	return manager
}

// observe records the page as the recorder would: the URL and the semantic
// elements, from the same snapshot path brw_snapshot serves.
func observe(t *testing.T, ctx context.Context, manager *browser.Manager) *TraceObservation {
	t.Helper()
	snap, err := manager.Snapshot(ctx, snapshot.SnapshotOptions{Limit: 200})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if strings.TrimSpace(snap.URL) == "" {
		t.Fatal("the snapshot carries no URL, so nothing could be compiled from it")
	}
	return &TraceObservation{URL: snap.URL, Elements: snap.Elements}
}

// findOne resolves exactly one element by role and accessible name. Anything
// else means the recording is ambiguous, and an ambiguous recording compiles
// into a recipe that acts on whichever element happened to come first.
func findOne(t *testing.T, observation *TraceObservation, role, name string) snapshot.Element {
	t.Helper()
	var matches []snapshot.Element
	for _, element := range observation.Elements {
		if element.Role == role && element.Name == name {
			matches = append(matches, element)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%s %q resolved to %d elements on %s", role, name, len(matches), observation.URL)
	}
	return matches[0]
}

// recordSignedInFlow drives the fixture in a real browser and returns the trace
// a recorder would have kept: one entry per action, with the page as it was
// before and after.
func recordSignedInFlow(t *testing.T, base string) []TraceStep {
	t.Helper()
	manager := liveChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, base+"/login"); err != nil {
		t.Fatalf("open the login page: %v", err)
	}
	login := observe(t, ctx, manager)
	steps := []TraceStep{{
		TraceAction: TraceAction{Action: "navigate_to", URL: base + "/login", OK: true},
		After:       login,
	}}

	act := func(before *TraceObservation, action TraceAction, run func() error) *TraceObservation {
		t.Helper()
		if err := run(); err != nil {
			t.Fatalf("%s: %v", action.Action, err)
		}
		after := observe(t, ctx, manager)
		action.OK = true
		steps = append(steps, TraceStep{TraceAction: action, Before: before, After: after})
		return after
	}

	signIn := findOne(t, login, "button", "Sign in")
	reports := act(login, TraceAction{
		Action: "click", Ref: signIn.Ref, Role: signIn.Role, Name: signIn.Name,
		NameIsVisibleText: signIn.NameIsVisibleText,
	}, func() error {
		_, err := manager.Click(ctx, signIn.Ref)
		return err
	})
	if !strings.Contains(reports.URL, "/reports") {
		t.Fatalf("signing in landed on %s, not the reports page", reports.URL)
	}

	field := findOne(t, reports, "textbox", "Report name")
	filled := act(reports, TraceAction{
		Action: "fill", Ref: field.Ref, Role: field.Role, Name: field.Name, Text: fixtureSearchTerm,
	}, func() error {
		_, err := manager.Fill(ctx, snapshot.FillOptions{Ref: field.Ref, Text: fixtureSearchTerm, Replace: true})
		return err
	})

	search := findOne(t, filled, "button", "Search reports")
	results := act(filled, TraceAction{
		Action: "click", Ref: search.Ref, Role: search.Role, Name: search.Name,
		NameIsVisibleText: search.NameIsVisibleText,
	}, func() error {
		_, err := manager.Click(ctx, search.Ref)
		return err
	})

	report := findOne(t, results, "link", "Quarterly revenue")
	opened := act(results, TraceAction{
		Action: "click", Ref: report.Ref, Role: report.Role, Name: report.Name,
		NameIsVisibleText: report.NameIsVisibleText,
	}, func() error {
		_, err := manager.Click(ctx, report.Ref)
		return err
	})
	if !strings.Contains(opened.URL, "/reports/quarterly-revenue") {
		t.Fatalf("opening the report landed on %s", opened.URL)
	}
	return steps
}

// replay runs the compiled recipe against a browser profile that has never seen
// this site.
func replay(t *testing.T, base string, value Recipe, inputs map[string]string) (RunResult, error) {
	t.Helper()
	manager := liveChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	// The runner checks the current origin before every step, including the
	// first, so a run cannot start on about:blank. This is the agent putting the
	// browser on the site; it carries no session, and the recipe's own first
	// step is still the navigation.
	if _, err := manager.Open(ctx, base+"/"); err != nil {
		t.Fatalf("open the fixture origin: %v", err)
	}
	surface := &BrowserSurface{Browser: manager}
	return Runner{Surface: surface, MaxDuration: 150 * time.Second}.Run(ctx, value, inputs)
}

// TestCompiledRecipeReplaysTwiceAgainstRealChromeOnASignedInFixture is the
// claim E8X4QH could not make: record against a real browser, compile, and
// replay the compiled recipe twice, each time on a profile with no cookies.
//
// The dropped-session run at the end is what stops the other two from proving
// nothing. If the fixture's pages rendered for an anonymous request, a replay
// with no session would pass and "signed-in" would be decoration.
func TestCompiledRecipeReplaysTwiceAgainstRealChromeOnASignedInFixture(t *testing.T) {
	site := &signedInSite{}
	server := httptest.NewServer(site.handler())
	defer server.Close()

	trace := recordSignedInFlow(t, server.URL)
	result, err := Compile(trace, CompileOptions{
		ID:          "fixture.reports.open-quarterly",
		Version:     "1.0.0",
		Name:        "Open the quarterly revenue report",
		Description: "Sign in to the fixture reporting site, search for a report and open it.",
		Intents:     []string{"open the quarterly revenue report"},
		Origins:     []string{server.URL},
		Risk:        "read_only",
	})
	if err != nil {
		t.Fatalf("compile the recorded flow: %v", err)
	}
	if len(result.Recipe.Inputs) != 1 {
		t.Fatalf("the compiled recipe declares %d inputs, want the one typed value", len(result.Recipe.Inputs))
	}
	inputs := map[string]string{}
	for name := range result.Recipe.Inputs {
		inputs[name] = fixtureSearchTerm
	}

	issuedBefore, refusalsBefore := site.counts()
	if issuedBefore != 1 {
		t.Fatalf("the recording pass signed in %d times, want once", issuedBefore)
	}

	for run := 1; run <= 2; run++ {
		outcome, err := replay(t, server.URL, result.Recipe, inputs)
		if err != nil {
			t.Fatalf("replay %d: %v", run, err)
		}
		if outcome.Status != "done" {
			t.Fatalf("replay %d status = %q", run, outcome.Status)
		}
		issued, refusals := site.counts()
		// A fresh profile has no cookie, so each replay signs in for itself.
		// Two replays that shared a session would show one login here.
		if issued != issuedBefore+run {
			t.Fatalf("after replay %d the fixture had issued %d sessions, want %d", run, issued, issuedBefore+run)
		}
		if refusals != refusalsBefore {
			t.Fatalf("replay %d hit %d unauthenticated refusals; it should have been signed in throughout",
				run, refusals-refusalsBefore)
		}
	}

	// Same compiled recipe, same fresh-profile replay, one difference: the
	// session cookie the site sets is one the browser discards.
	dropped := &signedInSite{dropCookie: true}
	droppedServer := httptest.NewUnstartedServer(dropped.handler())
	// The recipe pins the origin it was compiled against, so the second fixture
	// has to answer on the same address. httptest hands the listener back when
	// the first server is closed.
	server.Close()
	// The listener httptest opened for it is on some other port and is replaced,
	// so close it rather than leaving it bound for the rest of the run.
	_ = droppedServer.Listener.Close()
	droppedServer.Listener = mustListenOn(t, server.URL)
	droppedServer.Start()
	defer droppedServer.Close()

	outcome, err := replay(t, droppedServer.URL, result.Recipe, inputs)
	if err == nil {
		t.Fatal("a replay whose session cookie the browser dropped reported success; the pages it acted on cannot have needed the session")
	}
	t.Logf("dropped-session replay failed with: %v", err)
	if outcome.Status != "failed" {
		t.Fatalf("dropped-session replay status = %q, want failed", outcome.Status)
	}
	// Named so the failure cannot be a browser that never started or a fixture
	// that never answered: it is a recipe step that could not be performed.
	if !strings.Contains(err.Error(), "step \"s") {
		t.Fatalf("dropped-session failure is not a step failure: %v", err)
	}
	issued, refusals := dropped.counts()
	if issued == 0 {
		t.Fatal("the dropped-session replay never reached the login step")
	}
	if refusals == 0 {
		t.Fatal("the dropped-session replay never had a request refused, so it was not actually running without a session")
	}
}

// mustListenOn reopens the address a closed httptest server was using, so a
// second fixture can serve the exact origin a compiled recipe pins.
func mustListenOn(t *testing.T, serverURL string) net.Listener {
	t.Helper()
	address := strings.TrimPrefix(serverURL, "http://")
	var listener net.Listener
	var err error
	// The kernel can hold the port briefly after the first server closes.
	for attempt := 0; attempt < 50; attempt++ {
		listener, err = net.Listen("tcp", address)
		if err == nil {
			return listener
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reopen %s for the dropped-session fixture: %v", address, err)
	return nil
}
