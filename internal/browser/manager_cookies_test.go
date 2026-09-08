package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestCookieParamsValidate(t *testing.T) {
	cases := []struct {
		params CookieParams
		want   string
	}{
		{CookieParams{}, `action is required`},
		{CookieParams{Action: "bake"}, `unknown action "bake"`},
		{CookieParams{Action: "list"}, ""},
		{CookieParams{Action: "set"}, "name is required for set"},
		{CookieParams{Action: "set", Name: "sid"}, ""}, // tab origin can scope it
		{CookieParams{Action: "set", Name: "sid", URL: "https://example.test/"}, ""},
		{CookieParams{Action: "set", Name: "sid", Domain: "example.test"}, ""},
		{CookieParams{Action: "delete"}, "name is required for delete"},
		{CookieParams{Action: "delete", Name: "sid"}, ""}, // tab origin can scope it
		{CookieParams{Action: "delete", Name: "sid", Domain: "example.test"}, ""},
		// Action matching is case/whitespace tolerant — agents send "List".
		{CookieParams{Action: " List "}, ""},
	}
	for _, tc := range cases {
		err := tc.params.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%+v: unexpected error %v", tc.params, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: error = %v, want %q", tc.params, err, tc.want)
		}
	}
}

func TestCookieScopeURL(t *testing.T) {
	ok := func(in, want string) {
		t.Helper()
		got, err := cookieScopeURL(in)
		if err != nil {
			t.Fatalf("cookieScopeURL(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("cookieScopeURL(%q) = %q, want %q", in, got, want)
		}
	}
	ok("https://example.test/path", "https://example.test/path")
	ok("http://127.0.0.1:8080/", "http://127.0.0.1:8080/")
	ok("example.test", "https://example.test")
	ok("  example.test/a  ", "https://example.test/a")

	for _, bad := range []string{"", "file:///tmp/x.html", "about:blank", "://", "https:///nohost"} {
		if _, err := cookieScopeURL(bad); err == nil {
			t.Fatalf("cookieScopeURL(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestSameSiteFromUser(t *testing.T) {
	for in, want := range map[string]network.CookieSameSite{
		"":       "",
		"strict": network.CookieSameSiteStrict,
		"Lax":    network.CookieSameSiteLax,
		" none ": network.CookieSameSiteNone,
	} {
		got, set, err := sameSiteFromUser(in)
		if err != nil {
			t.Fatalf("sameSiteFromUser(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("sameSiteFromUser(%q) = %q, want %q", in, got, want)
		}
		if set != (want != "") {
			t.Fatalf("sameSiteFromUser(%q) set = %t", in, set)
		}
	}
	if _, _, err := sameSiteFromUser("sometimes"); err == nil {
		t.Fatal("unknown same_site unexpectedly accepted")
	}
}

func TestCookieDomainMatches(t *testing.T) {
	cases := []struct {
		cookie, host string
		want         bool
	}{
		{"example.test", "example.test", true},
		{".example.test", "example.test", true},
		{".example.test", "shop.example.test", true},
		{"shop.example.test", "example.test", false},
		{"other.test", "example.test", false},
		{"EXAMPLE.test", "example.TEST", true},
	}
	for _, tc := range cases {
		if got := cookieDomainMatches(tc.cookie, tc.host); got != tc.want {
			t.Errorf("cookieDomainMatches(%q, %q) = %t, want %t", tc.cookie, tc.host, got, tc.want)
		}
	}
}

func TestCookiePathOrDefault(t *testing.T) {
	if got := cookiePathOrDefault(""); got != "/" {
		t.Fatalf("empty path defaulted to %q, want /", got)
	}
	if got := cookiePathOrDefault(" /app "); got != "/app" {
		t.Fatalf("explicit path = %q, want /app", got)
	}
}

// The end-to-end contract: on a real (headless) Chrome, set/list/delete must
// round-trip through the CDP cookie store — including an HttpOnly cookie that
// document.cookie provably cannot see. Skipped when no local Chrome exists.
func TestManagerCookiesSetListDeleteIncludingHTTPOnly(t *testing.T) {
	m := newHeadlessManager(t)
	defer func() {
		if err := m.Close(); err != nil {
			t.Fatalf("close manager: %v", err)
		}
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Cookie Fixture</title><p>cookies</p>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	open, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabCtx := WithTabID(ctx, open.Tab.ID)

	// 1. Set a plain cookie scoped by URL.
	setPlain, err := m.Cookies(tabCtx, CookieParams{Action: "set", Name: "brw_plain", Value: "yes", URL: srv.URL, Path: "/"})
	if err != nil {
		t.Fatalf("set plain: %v", err)
	}
	if setPlain.Cookie == nil || setPlain.Cookie.Value != "yes" {
		t.Fatalf("set plain did not read back: %+v", setPlain)
	}

	// 2. Set an HttpOnly cookie — the capability document.cookie lacks.
	setHTTPOnly, err := m.Cookies(tabCtx, CookieParams{Action: "set", Name: "brw_httponly", Value: "secret", URL: srv.URL, HTTPOnly: true})
	if err != nil {
		t.Fatalf("set httponly: %v", err)
	}
	if setHTTPOnly.Cookie == nil || !setHTTPOnly.Cookie.HTTPOnly {
		t.Fatalf("httponly cookie not stored as HttpOnly: %+v", setHTTPOnly)
	}

	// 3. Prove the HttpOnly cookie is invisible to the page: evaluate
	// document.cookie and require brw_httponly to be absent while brw_plain is
	// present.
	pageCookie, err := m.Evaluate(tabCtx, `document.cookie`)
	if err != nil {
		t.Fatalf("evaluate document.cookie: %v", err)
	}
	pageStr, _ := pageCookie.(string)
	if strings.Contains(pageStr, "brw_httponly") {
		t.Fatalf("HttpOnly cookie leaked into document.cookie: %q", pageStr)
	}
	if !strings.Contains(pageStr, "brw_plain") {
		t.Fatalf("plain cookie missing from document.cookie: %q", pageStr)
	}

	// 4. List must return BOTH (that invisibility is the point of the tool).
	listed, err := m.Cookies(tabCtx, CookieParams{Action: "list", URL: srv.URL})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var sawPlain, sawHTTPOnly bool
	for _, c := range listed.Cookies {
		switch c.Name {
		case "brw_plain":
			sawPlain = !c.HTTPOnly
		case "brw_httponly":
			sawHTTPOnly = c.HTTPOnly
		}
	}
	if !sawPlain || !sawHTTPOnly {
		t.Fatalf("list missed cookies (plain=%t httponly=%t): %+v", sawPlain, sawHTTPOnly, listed.Cookies)
	}

	// 5. Name-filtered list narrows to exactly the match.
	only, err := m.Cookies(tabCtx, CookieParams{Action: "list", URL: srv.URL, Name: "brw_httponly"})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if only.Count != 1 || only.Cookies[0].Name != "brw_httponly" {
		t.Fatalf("name filter returned %+v", only)
	}

	// 6. Delete the plain cookie; the HttpOnly one must survive.
	deleted, err := m.Cookies(tabCtx, CookieParams{Action: "delete", Name: "brw_plain", URL: srv.URL})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.RemainingSameName != 0 {
		t.Fatalf("plain cookie still present after delete: %+v", deleted)
	}
	after, err := m.Cookies(tabCtx, CookieParams{Action: "list", URL: srv.URL})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	for _, c := range after.Cookies {
		if c.Name == "brw_plain" {
			t.Fatalf("brw_plain survived deletion: %+v", after.Cookies)
		}
	}
}

// Setting a cookie on a non-http(s) scope must fail with a clear message
// before any CDP round-trip.
func TestManagerCookiesRejectsNonHTTPScope(t *testing.T) {
	m := newHeadlessManager(t)
	defer func() {
		if err := m.Close(); err != nil {
			t.Fatalf("close manager: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = m.Cookies(WithTabID(ctx, open.Tab.ID), CookieParams{Action: "list", URL: "file:///tmp/nope.html"})
	if err == nil || !(strings.Contains(err.Error(), "http(s)") || strings.Contains(err.Error(), "no host")) {
		t.Fatalf("expected http(s)-scope error, got %v", err)
	}
	// And a tab with no navigated origin asks for an explicit url.
	_, err = m.Cookies(WithTabID(ctx, open.Tab.ID), CookieParams{Action: "list"})
	if err == nil || !strings.Contains(err.Error(), "no navigated origin") {
		t.Fatalf("expected no-origin error, got %v", err)
	}
}
