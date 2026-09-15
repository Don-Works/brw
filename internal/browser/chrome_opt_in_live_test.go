package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/chromeoptin"
)

// The Chrome opt-in lane is discovery plus attach plus browser-target CDP, and
// only the last two can be exercised without a person clicking the switch in
// chrome://inspect. This test drives all three against a real Chrome by
// producing the same endpoint shape the opt-in produces: a live DevTools server
// on a dynamically chosen port, recorded in the user data directory's
// DevToolsActivePort file, exposing a browser target.
//
// What it therefore establishes is that brw's discovery finds that endpoint,
// attaches to it, and runs the two things the extension bridge cannot — an
// incognito context and an HttpOnly cookie read. What it does not establish is
// the human action; see docs/install.md.
func TestChromeOptInLaneDrivesWhatTheBridgeCannot(t *testing.T) {
	if _, err := cdplaunch.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userDataDir, chosenPort := startChromeOutsideBrw(ctx, t, "")

	// Discovery: brw is told only the user data directory, exactly as it is on
	// the real lane. The port is not passed in and cannot be guessed.
	endpoint, err := chromeoptin.Discover(ctx, chromeoptin.Options{UserDataDir: userDataDir})
	if err != nil {
		t.Fatalf("discovery found no endpoint for a browser that has one: %v", err)
	}
	if endpoint.Port != chosenPort {
		t.Fatalf("discovered port %d, but the browser is on %d", endpoint.Port, chosenPort)
	}
	if endpoint.BrowserWSURL == "" {
		t.Fatal("discovery reported no browser target, which is the whole capability this lane adds")
	}

	manager, err := New(ctx, Config{
		RemoteURL:       endpoint.HTTPURL,
		AttachOnly:      true,
		SignedInProfile: true,
		Timeout:         20 * time.Second,
	})
	if err != nil {
		t.Fatalf("attach to the discovered endpoint: %v", err)
	}
	defer manager.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "brw_opt_in_httponly",
			Value:    "fixture-value",
			Path:     "/",
			HttpOnly: true,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>opt-in fixture</title><p id="body">opt-in fixture</p>`))
	}))
	defer srv.Close()

	// Incognito: a separate browser context, which needs the CDP browser
	// target. The extension bridge returns ErrIncognitoUnsupported here.
	opened, err := manager.OpenIncognito(ctx, srv.URL+"/")
	if err != nil {
		t.Fatalf("open an incognito context over the opt-in endpoint: %v", err)
	}
	if opened.Tab.ID == "" {
		t.Fatal("incognito open returned no tab")
	}

	// HttpOnly cookies: readable at the browser-context level, and blocked on
	// the bridge by the extension's own security policy.
	cookies, err := manager.Cookies(WithTabID(ctx, opened.Tab.ID), CookieParams{
		Action: CookieActionList,
		URL:    srv.URL + "/",
	})
	if err != nil {
		t.Fatalf("list cookies over the opt-in endpoint: %v", err)
	}
	var found *Cookie
	for i := range cookies.Cookies {
		if cookies.Cookies[i].Name == "brw_opt_in_httponly" {
			found = &cookies.Cookies[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the HttpOnly cookie the fixture set was not readable: %+v", cookies.Cookies)
	}
	if !found.HTTPOnly {
		t.Fatalf("cookie %s came back with http_only=false; the fixture set it HttpOnly", found.Name)
	}
	if found.Value != "fixture-value" {
		t.Fatalf("cookie value = %q", found.Value)
	}

	// And the refusal that comes with the lane: this is the browser a user is
	// signed into, so brw_state will not seal its cookies even though the CDP
	// to do so is right there.
	if _, err := manager.SessionState(ctx, SessionStateOptions{Action: SessionStateActionList}); !errors.Is(err, ErrSessionStateSignedIn) {
		t.Fatalf("brw_state on the opt-in lane = %v, want ErrSessionStateSignedIn", err)
	}
}
