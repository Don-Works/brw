package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/sessionstate"
)

// Fabricated fixture material. Low entropy on purpose: these are strings a test
// greps for, not credentials.
const (
	fixtureStateKey         = "fixture-session-state-key-for-tests-0001"
	fixtureStateCookieName  = "fixture-session-cookie-one"
	fixtureStateCookieValue = "fixture-session-value-one"
)

func newStateStore(t *testing.T) *sessionstate.Store {
	t.Helper()
	store, err := sessionstate.NewStore(sessionstate.Config{
		Root: filepath.Join(t.TempDir(), "state"),
		Key:  []byte(fixtureStateKey),
	})
	if err != nil {
		t.Fatalf("sessionstate.NewStore: %v", err)
	}
	return store
}

func TestSessionStateOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		opts SessionStateOptions
		want string
	}{
		{name: "save needs origins", opts: SessionStateOptions{Action: "save"}, want: "origins is required"},
		{name: "save with origins", opts: SessionStateOptions{Action: "save", Origins: []string{"https://app.example.test"}}},
		{name: "restore needs an id", opts: SessionStateOptions{Action: "restore"}, want: "snapshot_id is required for restore"},
		{name: "delete needs an id", opts: SessionStateOptions{Action: "delete"}, want: "snapshot_id is required for delete"},
		{name: "list needs nothing", opts: SessionStateOptions{Action: "list"}},
		{name: "case and space tolerant", opts: SessionStateOptions{Action: " List "}},
		{name: "missing action", opts: SessionStateOptions{}, want: "action is required"},
		{name: "unknown action", opts: SessionStateOptions{Action: "export"}, want: `unknown action "export"`},
		{name: "negative ttl", opts: SessionStateOptions{Action: "list", TTLSeconds: -1}, want: "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// A daemon started without a key must refuse rather than write a signed-in
// session in the clear, and it must say which flag turns the feature on.
func TestSessionStateRefusesWithoutAStore(t *testing.T) {
	m := &Manager{}
	_, err := m.SessionState(context.Background(), SessionStateOptions{Action: "list"})
	if !errors.Is(err, ErrSessionStateDisabled) {
		t.Fatalf("SessionState = %v, want ErrSessionStateDisabled", err)
	}
	if !strings.Contains(err.Error(), "--state-key-file") {
		t.Fatalf("the refusal must name the flag that enables snapshots, got %q", err)
	}
}

// Everything brw_state returns has to be safe to put in a model context. This
// drives the real result path (Manager.SessionState over a real store) and
// fails the moment a payload-bearing field is added to the result.
func TestSessionStateResultsCarryNoCookieMaterial(t *testing.T) {
	store := newStateStore(t)
	meta, err := store.Save(sessionstate.Snapshot{
		Origins: []string{"https://app.example.test"},
		Cookies: []sessionstate.Cookie{{
			Name: fixtureStateCookieName, Value: fixtureStateCookieValue,
			Domain: "app.example.test", Path: "/",
		}},
	}, sessionstate.SaveOptions{})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	m := &Manager{}
	m.SetSessionStateStore(store)
	result, err := m.SessionState(context.Background(), SessionStateOptions{Action: SessionStateActionList})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(result.Snapshots) != 1 || result.Snapshots[0].CookieCount != 1 {
		t.Fatalf("list = %+v, want one snapshot holding one cookie", result.Snapshots)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{fixtureStateCookieName, fixtureStateCookieValue} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("a brw_state result emitted %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), meta.ID) {
		t.Fatalf("the result must carry the snapshot id so a restore can name it: %s", encoded)
	}
}

// The end-to-end contract on a real headless Chrome: a signed-in incognito
// context is sealed, thrown away, and a brand-new context starts signed in.
// Skipped when no local Chrome exists.
func TestSessionStateRoundTripsAcrossIncognitoContexts(t *testing.T) {
	m := newHeadlessManager(t)
	m.SetSessionStateStore(newStateStore(t))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{
				Name: fixtureStateCookieName, Value: fixtureStateCookieValue,
				Path: "/", HttpOnly: true,
			})
		}
		who := "anonymous"
		if cookie, err := r.Cookie(fixtureStateCookieName); err == nil && cookie.Value == fixtureStateCookieValue {
			who = "signed-in"
		}
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>State Fixture</title><p id="who">` + who + `</p>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	firstContext := signInIncognito(t, m, ctx, srv.URL)
	saved, err := m.SessionState(ctx, SessionStateOptions{
		Action:    SessionStateActionSave,
		Origins:   []string{srv.URL},
		ContextID: firstContext,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Snapshot == nil || saved.Snapshot.CookieCount != 1 {
		t.Fatalf("save = %+v, want exactly the one session cookie", saved)
	}
	if err := m.CloseContext(ctx, firstContext); err != nil {
		t.Fatalf("close first context: %v", err)
	}

	// A brand-new isolated context shares nothing, so it must start signed out.
	fresh, err := m.OpenIncognito(ctx, srv.URL+"/whoami")
	if err != nil {
		t.Fatalf("open second incognito: %v", err)
	}
	if got := whoAmI(t, m, ctx); got != "anonymous" {
		t.Fatalf("a fresh incognito context reported %q, want anonymous", got)
	}

	restored, err := m.SessionState(ctx, SessionStateOptions{
		Action:     SessionStateActionRestore,
		SnapshotID: saved.Snapshot.ID,
		ContextID:  fresh.Tab.BrowserContextID,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.RestoredCookies != 1 {
		t.Fatalf("restore applied %d cookies, want 1", restored.RestoredCookies)
	}
	if _, err := m.NavigateTo(ctx, srv.URL+"/whoami"); err != nil {
		t.Fatalf("navigate after restore: %v", err)
	}
	if got := whoAmI(t, m, ctx); got != "signed-in" {
		t.Fatalf("after restore the page reported %q, want signed-in", got)
	}
	if err := m.CloseContext(ctx, fresh.Tab.BrowserContextID); err != nil {
		t.Fatalf("close second context: %v", err)
	}
}

// The allowlist is applied again on the restore side, so a snapshot carrying a
// cookie for some other origin cannot install it. This seals a hostile snapshot
// directly through the store — the capture-side filter never sees it — and then
// asks the live browser what it actually holds.
func TestRestoreRefusesToInstallAnOffAllowlistCookie(t *testing.T) {
	m := newHeadlessManager(t)
	store := newStateStore(t)
	m.SetSessionStateStore(store)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Allowlist Fixture</title><p>page</p>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	meta, err := store.Save(sessionstate.Snapshot{
		Origins: []string{srv.URL, "https://other.example.test"},
		Cookies: []sessionstate.Cookie{
			{Name: fixtureStateCookieName, Value: fixtureStateCookieValue, Domain: host, Path: "/"},
			{Name: "fixture-foreign-cookie-one", Value: "fixture-foreign-value-one", Domain: "other.example.test", Path: "/"},
		},
	}, sessionstate.SaveOptions{})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	opened, err := m.OpenIncognito(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open incognito: %v", err)
	}
	defer func() { _ = m.CloseContext(context.Background(), opened.Tab.BrowserContextID) }()

	restored, err := m.SessionState(ctx, SessionStateOptions{
		Action:     SessionStateActionRestore,
		SnapshotID: meta.ID,
		Origins:    []string{srv.URL},
		ContextID:  opened.Tab.BrowserContextID,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.RestoredCookies != 1 {
		t.Fatalf("restore applied %d cookies, want only the allowlisted one", restored.RestoredCookies)
	}
	if restored.SkippedOffAllowlist != 1 {
		t.Fatalf("skipped_off_allowlist = %d, want 1 — the caller has to be told material was refused", restored.SkippedOffAllowlist)
	}
	cookies, err := m.contextCookies(ctx, opened.Tab.BrowserContextID)
	if err != nil {
		t.Fatalf("read back the context's cookies: %v", err)
	}
	for _, cookie := range cookies {
		if strings.Contains(cookie.Domain, "other.example.test") {
			t.Fatalf("the restore installed an off-allowlist cookie on %q", cookie.Domain)
		}
	}
}

func signInIncognito(t *testing.T, m *Manager, ctx context.Context, base string) string {
	t.Helper()
	opened, err := m.OpenIncognito(ctx, base+"/login")
	if err != nil {
		t.Fatalf("open incognito: %v", err)
	}
	if opened.Tab.BrowserContextID == "" {
		t.Fatal("incognito open returned no context id")
	}
	if got := whoAmI(t, m, ctx); got != "anonymous" {
		// The login response sets the cookie; the page rendering it was still
		// the unauthenticated one, so this is the expected first read.
		t.Logf("login page reported %q", got)
	}
	if _, err := m.NavigateTo(ctx, base+"/whoami"); err != nil {
		t.Fatalf("navigate after login: %v", err)
	}
	if got := whoAmI(t, m, ctx); got != "signed-in" {
		t.Fatalf("the fixture did not sign in: page reported %q", got)
	}
	return opened.Tab.BrowserContextID
}

func whoAmI(t *testing.T, m *Manager, ctx context.Context) string {
	t.Helper()
	value, err := m.Evaluate(ctx, `document.getElementById('who') ? document.getElementById('who').textContent : ''`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
