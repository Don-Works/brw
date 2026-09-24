package browser

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAuthChallenge(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   *AuthChallenge
	}{
		{"none", nil, nil},
		{"empty value", []string{""}, nil},
		{"vercel preview", []string{`Basic realm="Preview"`}, &AuthChallenge{Scheme: "Basic", Realm: "Preview"}},
		{"lowercase scheme and token realm", []string{`basic realm=intranet`}, &AuthChallenge{Scheme: "Basic", Realm: "intranet"}},
		{"digest keeps its first realm", []string{`Digest realm="ops@example.test", qop="auth", nonce="abc"`}, &AuthChallenge{Scheme: "Digest", Realm: "ops@example.test"}},
		{"escaped quote in realm", []string{`Basic realm="say \"hi\""`}, &AuthChallenge{Scheme: "Basic", Realm: `say "hi"`}},
		{"no realm", []string{`Negotiate`}, &AuthChallenge{Scheme: "Negotiate"}},
		{"browser-answerable challenge preferred in one header", []string{`Bearer realm="api", Basic realm="site"`}, &AuthChallenge{Scheme: "Basic", Realm: "site"}},
		{"browser-answerable challenge preferred across headers", []string{`Bearer realm="api"`, `NTLM`}, &AuthChallenge{Scheme: "NTLM"}},
		{"bearer only", []string{`Bearer realm="api", error="invalid_token"`}, &AuthChallenge{Scheme: "Bearer", Realm: "api"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseAuthChallenge(tt.values); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseAuthChallenge(%q) = %+v, want %+v", tt.values, got, tt.want)
			}
		})
	}
}

func TestNavigationOutcomeFailed(t *testing.T) {
	tests := []struct {
		name    string
		outcome NavigationOutcome
		want    bool
	}{
		{"loaded page", NavigationOutcome{CommittedURL: "https://site.test/"}, false},
		{"rendered 404", NavigationOutcome{CommittedURL: "https://site.test/x", HTTPStatus: 404}, false},
		{"error page without a code", NavigationOutcome{CommittedURL: ErrorPageURL}, true},
		{"net error", NavigationOutcome{Error: "net::ERR_NAME_NOT_RESOLVED"}, true},
		{"auth cancelled", NavigationOutcome{Error: "net::ERR_INVALID_AUTH_CREDENTIALS", HTTPStatus: 401}, true},
		{"aborted keeps the previous document", NavigationOutcome{Error: "net::ERR_ABORTED"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.outcome.Failed(); got != tt.want {
				t.Fatalf("Failed() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestNavigationOutcomeDescribeNamesTheFix(t *testing.T) {
	basic := &AuthChallenge{Scheme: "Basic", Realm: "Preview"}
	tests := []struct {
		name          string
		outcome       NavigationOutcome
		authAvailable bool
		want          []string
		notWant       []string
	}{
		{
			name: "basic challenge on the extension bridge",
			outcome: NavigationOutcome{URL: "https://preview.test/", CommittedURL: ErrorPageURL,
				Error: "net::ERR_INVALID_AUTH_CREDENTIALS", HTTPStatus: 401, AuthRequired: basic},
			want: []string{"open: navigation failed: https://preview.test/ answered HTTP 401",
				"net::ERR_INVALID_AUTH_CREDENTIALS", "chrome-error://chromewebdata/",
				`HTTP Basic authentication (realm "Preview")`,
				"brw_authenticate is not available on the extension bridge", "CDP lane"},
			notWant: []string{"Call brw_authenticate"},
		},
		{
			name: "basic challenge on a CDP lane",
			outcome: NavigationOutcome{URL: "https://preview.test/", Error: "net::ERR_INVALID_AUTH_CREDENTIALS",
				HTTPStatus: 401, AuthRequired: basic},
			authAvailable: true,
			want:          []string{"Call brw_authenticate", `realm "Preview"`},
			notWant:       []string{"CDP lane"},
		},
		{
			name:    "auth error from an extension without navigation_outcome",
			outcome: NavigationOutcome{URL: "https://preview.test/", Error: "net::ERR_INVALID_AUTH_CREDENTIALS"},
			want:    []string{"did not load", "asks for HTTP authentication", "CDP lane"},
		},
		{
			name: "bearer challenge is not a browser credential",
			outcome: NavigationOutcome{URL: "https://api.test/", CommittedURL: ErrorPageURL, HTTPStatus: 401,
				AuthRequired: &AuthChallenge{Scheme: "Bearer", Realm: "api"}},
			want:    []string{"HTTP Bearer authentication", "site's own sign-in flow"},
			notWant: []string{"brw_authenticate"},
		},
		{
			name:    "dns failure",
			outcome: NavigationOutcome{URL: "https://nowhere.test/", CommittedURL: ErrorPageURL, Error: "net::ERR_NAME_NOT_RESOLVED"},
			want:    []string{"https://nowhere.test/ did not load (net::ERR_NAME_NOT_RESOLVED)", "not the site"},
			notWant: []string{"authentication", "brw_authenticate"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.outcome.Describe("open", tt.authAvailable)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Describe() = %q, missing %q", got, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("Describe() = %q, must not contain %q", got, notWant)
				}
			}
			if !IsNavigationFailedMessage(got) {
				t.Errorf("Describe() = %q does not carry the navigation-failed marker", got)
			}
		})
	}
}

func TestApplyNavigationOutcome(t *testing.T) {
	basic := &AuthChallenge{Scheme: "Basic", Realm: "Preview"}
	tests := []struct {
		name      string
		outcome   NavigationOutcome
		wantReady bool
		wantError string
		wantHTTP  int
		wantAuth  *AuthChallenge
	}{
		{"loaded page keeps ready", NavigationOutcome{URL: "https://ok.test/", CommittedURL: "https://ok.test/", HTTPStatus: 200}, true, "", 0, nil},
		{"rendered 404 keeps ready and reports the status", NavigationOutcome{URL: "https://ok.test/x", CommittedURL: "https://ok.test/x", HTTPStatus: 404}, true, "", 404, nil},
		{"auth challenge", NavigationOutcome{URL: "https://p.test/", CommittedURL: ErrorPageURL, Error: "net::ERR_INVALID_AUTH_CREDENTIALS", HTTPStatus: 401, AuthRequired: basic}, false, "net::ERR_INVALID_AUTH_CREDENTIALS", 401, basic},
		{"error page with no code", NavigationOutcome{URL: "https://p.test/", CommittedURL: ErrorPageURL}, false, "chrome-error page (the browser reported no error code)", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := OpenResult{Tab: Tab{ID: "7"}, Ready: true}
			result.ApplyNavigationOutcome(tt.outcome, false)
			if result.Ready != tt.wantReady || result.NavigationError != tt.wantError || result.HTTPStatus != tt.wantHTTP ||
				!reflect.DeepEqual(result.AuthRequired, tt.wantAuth) {
				t.Fatalf("result = %+v, want ready=%t navigation_error=%q http_status=%d auth=%+v",
					result, tt.wantReady, tt.wantError, tt.wantHTTP, tt.wantAuth)
			}
			failed := result.NavigationErr() != nil
			if failed != !tt.wantReady || (failed && result.Warning == "") || (!failed && result.Warning != "") {
				t.Fatalf("NavigationErr() = %v, warning = %q", result.NavigationErr(), result.Warning)
			}
		})
	}
}
