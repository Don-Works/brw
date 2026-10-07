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

const (
	fixtureSessionCookie = "brw_fixture_session"
	fixtureOperator      = "Signed in as fixture operator"
	fixtureSearchTerm    = "quarterly"
)

type signedInSite struct {
	mu sync.Mutex

	sessions []string

	dropCookie bool

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

func replay(t *testing.T, base string, value Recipe, inputs map[string]string) (RunResult, error) {
	t.Helper()
	manager := liveChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, base+"/"); err != nil {
		t.Fatalf("open the fixture origin: %v", err)
	}
	surface := &BrowserSurface{Browser: manager}
	return Runner{Surface: surface, MaxDuration: 150 * time.Second}.Run(ctx, value, inputs)
}

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

		if issued != issuedBefore+run {
			t.Fatalf("after replay %d the fixture had issued %d sessions, want %d", run, issued, issuedBefore+run)
		}
		if refusals != refusalsBefore {
			t.Fatalf("replay %d hit %d unauthenticated refusals; it should have been signed in throughout",
				run, refusals-refusalsBefore)
		}
	}

	dropped := &signedInSite{dropCookie: true}
	droppedServer := httptest.NewUnstartedServer(dropped.handler())

	server.Close()

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

func mustListenOn(t *testing.T, serverURL string) net.Listener {
	t.Helper()
	address := strings.TrimPrefix(serverURL, "http://")
	var listener net.Listener
	var err error

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
