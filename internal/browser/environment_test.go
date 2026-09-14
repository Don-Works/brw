package browser

import (
	"reflect"
	"strings"
	"testing"
)

// Origin comparison decides which requests get a bearer token, so two spellings
// of one origin must not be two origins, and two different origins must never
// collapse into one.
func TestCanonicalOrigin(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain origin", input: "https://api.example.com", want: "https://api.example.com"},
		{name: "trailing slash", input: "https://api.example.com/", want: "https://api.example.com"},
		{name: "path and query are dropped", input: "https://api.example.com/v1/things?page=2", want: "https://api.example.com"},
		{name: "host case is normalized", input: "https://API.Example.COM", want: "https://api.example.com"},
		{name: "default https port is removed", input: "https://api.example.com:443", want: "https://api.example.com"},
		{name: "default http port is removed", input: "http://api.example.com:80", want: "http://api.example.com"},
		{name: "a non-default port is kept", input: "http://127.0.0.1:8080/x", want: "http://127.0.0.1:8080"},
		{name: "an IPv6 literal keeps its brackets", input: "http://[::1]:8080", want: "http://[::1]:8080"},
		{name: "scheme is part of the origin", input: "http://api.example.com", want: "http://api.example.com"},
		{name: "empty", input: "  ", wantErr: true},
		{name: "no scheme", input: "api.example.com", wantErr: true},
		{name: "a scheme that is not http", input: "ftp://api.example.com", wantErr: true},
		{name: "file URL", input: "file:///etc/passwd", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalOrigin(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CanonicalOrigin(%q) = %q, want an error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalOrigin(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("CanonicalOrigin(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeExtraHeaders(t *testing.T) {
	tests := []struct {
		name    string
		opts    ExtraHeadersOptions
		wantErr string
	}{
		{
			name: "a valid entry",
			opts: ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Authorization": "Bearer fabricated"}}}},
		},
		{name: "no origins", opts: ExtraHeadersOptions{}, wantErr: "at least one origin"},
		{
			name:    "an origin with no headers",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com"}}},
			wantErr: "declares no headers",
		},
		{
			name: "the same origin twice",
			opts: ExtraHeadersOptions{Origins: []OriginHeaders{
				{Origin: "https://api.example.com", Headers: map[string]string{"A": "1"}},
				{Origin: "https://api.example.com:443/", Headers: map[string]string{"B": "2"}},
			}},
			wantErr: "declared twice",
		},
		{
			name:    "a header name that is not a token",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Bad Header": "1"}}}},
			wantErr: "not a valid header-name character",
		},
		{
			// A newline in a value is a second header the caller never declared,
			// on an origin they did declare.
			name:    "a value carrying a newline",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"X-Test": "a\r\nX-Injected: b"}}}},
			wantErr: "newline",
		},
		{
			// Host is evaluated by containmentVerdict out of the request URL, so a
			// forged one reaches a virtual host the policy was asked to confine.
			name:    "host",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Host": "internal.example.test"}}}},
			wantErr: "cannot be set from the per-origin header table",
		},
		{
			name:    "host in any spelling",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"hOsT": "internal.example.test"}}}},
			wantErr: "cannot be set from the per-origin header table",
		},
		{
			name:    "content-length",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Content-Length": "0"}}}},
			wantErr: "cannot be set from the per-origin header table",
		},
		{
			name:    "transfer-encoding",
			opts:    ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Transfer-Encoding": "chunked"}}}},
			wantErr: "cannot be set from the per-origin header table",
		},
		{
			// Content-Type describes the body the page already built; it forges
			// nothing and is one of the common reasons to declare a header at all.
			name: "content-type is still allowed",
			opts: ExtraHeadersOptions{Origins: []OriginHeaders{{Origin: "https://api.example.com", Headers: map[string]string{"Content-Type": "application/json"}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NormalizeExtraHeaders(tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NormalizeExtraHeaders: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// The echo is what lands in an agent transcript, so it must carry names only.
func TestHeaderNamesWithholdsValues(t *testing.T) {
	entries := []OriginHeaders{{
		Origin:  "https://api.example.com",
		Headers: map[string]string{"Authorization": "Bearer fabricated-token-value", "X-Trace": "on"},
	}}
	echoed := HeaderNames(entries)
	if len(echoed) != 1 {
		t.Fatalf("got %d entries, want 1", len(echoed))
	}
	if !reflect.DeepEqual(echoed[0].Headers, []string{"Authorization", "X-Trace"}) {
		t.Fatalf("header names = %v, want them sorted and complete", echoed[0].Headers)
	}
	for _, name := range echoed[0].Headers {
		if strings.Contains(name, "fabricated-token-value") {
			t.Fatal("a header value leaked into the echoed names")
		}
	}
}

// A credential armed for one origin must not be armed for a navigation to
// another, or the password is offered to whoever the URL happens to point at.
func TestNormalizeCredentialsKeepsTheURLInsideTheOrigin(t *testing.T) {
	tests := []struct {
		name       string
		opts       CredentialsOptions
		wantTarget string
		wantErr    string
	}{
		{
			name:       "no url loads the origin",
			opts:       CredentialsOptions{Origin: "https://staging.example.com", Username: "u", Password: "p"},
			wantTarget: "https://staging.example.com/",
		},
		{
			name:       "a url inside the origin",
			opts:       CredentialsOptions{Origin: "https://staging.example.com", Username: "u", Password: "p", URL: "https://staging.example.com/reports/7"},
			wantTarget: "https://staging.example.com/reports/7",
		},
		{
			name:    "a url on another host",
			opts:    CredentialsOptions{Origin: "https://staging.example.com", Username: "u", Password: "p", URL: "https://evil.example.net/collect"},
			wantErr: "not the declared credential origin",
		},
		{
			name:    "a url on another scheme",
			opts:    CredentialsOptions{Origin: "https://staging.example.com", Username: "u", Password: "p", URL: "http://staging.example.com/"},
			wantErr: "not the declared credential origin",
		},
		{
			name:    "no username and no password",
			opts:    CredentialsOptions{Origin: "https://staging.example.com"},
			wantErr: "need a username or a password",
		},
		{
			name:    "a password carrying a newline",
			opts:    CredentialsOptions{Origin: "https://staging.example.com", Username: "u", Password: "p\r\n"},
			wantErr: "must not contain newlines",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, target, err := NormalizeCredentials(tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeCredentials: %v", err)
			}
			if target != tt.wantTarget {
				t.Fatalf("target = %q, want %q", target, tt.wantTarget)
			}
		})
	}
}

// headersFor is the scoping decision itself, made once per intercepted request.
func TestHeadersForMatchesOnlyTheDeclaredOrigin(t *testing.T) {
	var env environmentState
	env.setHeaders("tab-1", []OriginHeaders{
		{Origin: "https://api.example.com", Headers: map[string]string{"Authorization": "Bearer fabricated"}},
		{Origin: "http://127.0.0.1:8080", Headers: map[string]string{"X-Local": "1"}},
	})

	tests := []struct {
		name       string
		tabID      string
		requestURL string
		wantHeader string
	}{
		{"the declared origin", "tab-1", "https://api.example.com/v1/things", "Authorization"},
		{"the declared origin on its default port", "tab-1", "https://api.example.com:443/v1", "Authorization"},
		{"a second declared origin", "tab-1", "http://127.0.0.1:8080/health", "X-Local"},
		{"a different host", "tab-1", "https://cdn.example.com/font.woff2", ""},
		{"a subdomain is a different origin", "tab-1", "https://inner.api.example.com/v1", ""},
		{"the same host on another scheme", "tab-1", "http://api.example.com/v1", ""},
		{"the same host on another port", "tab-1", "http://127.0.0.1:9090/health", ""},
		{"another tab", "tab-2", "https://api.example.com/v1/things", ""},
		{"a data URL", "tab-1", "data:text/html,x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := env.headersFor(tt.tabID, tt.requestURL)
			if tt.wantHeader == "" {
				if len(got) != 0 {
					t.Fatalf("headersFor(%q) = %v, want no headers", tt.requestURL, got)
				}
				return
			}
			if _, ok := got[tt.wantHeader]; !ok {
				t.Fatalf("headersFor(%q) = %v, want it to carry %q", tt.requestURL, got, tt.wantHeader)
			}
		})
	}
}

// dropCredential is what the "nothing is retained" guarantee rests on, and
// holdsSecret is what the live test uses to check it. Prove both here, where the
// before/after can be compared without a browser.
func TestDropCredentialLeavesNothingBehind(t *testing.T) {
	const fixtureCredential = "fabricated-password-4c81"
	var env environmentState
	env.armCredential("tab-1", "https://staging.example.com", "user", fixtureCredential)

	if !holdsSecret(reflect.ValueOf(&env).Elem(), fixtureCredential, map[uintptr]bool{}) {
		t.Fatal("an armed credential is not reachable by the scan, so the live test's retention assertion would pass vacuously")
	}
	if !env.authArmed("tab-1") {
		t.Fatal("authArmed is false while a credential is armed, so Fetch.enable would never ask for auth events")
	}

	challenged, answered := env.dropCredential("tab-1")
	if challenged || answered != 0 {
		t.Fatalf("outcome = (%v, %d), want an unused credential", challenged, answered)
	}
	if holdsSecret(reflect.ValueOf(&env).Elem(), fixtureCredential, map[uintptr]bool{}) {
		t.Fatal("the password is still reachable after dropCredential")
	}
	if env.authArmed("tab-1") {
		t.Fatal("auth handling is still armed after dropCredential")
	}
}

// A challenge from an origin the caller did not name must not be answered with
// their password, however the redirect that produced it happened.
func TestAuthResponseOnlyAnswersTheDeclaredOrigin(t *testing.T) {
	const fixtureCredential = "fabricated-password-77a1"
	var env environmentState
	env.armCredential("tab-1", "https://staging.example.com", "user", fixtureCredential)

	other := env.authResponse("tab-1", "https://evil.example.net/collect")
	if other.Response != "Default" {
		t.Fatalf("response for an undeclared origin = %q, want Default", other.Response)
	}
	if other.Password != "" {
		t.Fatal("the password was offered to an undeclared origin")
	}

	declared := env.authResponse("tab-1", "https://staging.example.com/reports")
	if declared.Response != "ProvideCredentials" || declared.Password != fixtureCredential {
		t.Fatalf("response for the declared origin = %+v, want the credentials", declared.Response)
	}

	challenged, answered := env.dropCredential("tab-1")
	if !challenged {
		t.Fatal("a challenge from an undeclared origin should still be recorded as a challenge")
	}
	if answered != 1 {
		t.Fatalf("answered = %d, want exactly the one challenge from the declared origin", answered)
	}
}

func TestNormalizeMediaEmulationRejectsUnknownValues(t *testing.T) {
	tests := []struct {
		name    string
		opts    MediaEmulationOptions
		wantErr bool
	}{
		{name: "color scheme alone", opts: MediaEmulationOptions{ColorScheme: "dark"}},
		{name: "media alone", opts: MediaEmulationOptions{Media: "print"}},
		{name: "case is normalized", opts: MediaEmulationOptions{ColorScheme: "DARK"}},
		{name: "clear needs nothing else", opts: MediaEmulationOptions{Clear: true}},
		{name: "nothing at all", opts: MediaEmulationOptions{}, wantErr: true},
		{name: "an unknown media type", opts: MediaEmulationOptions{Media: "braille"}, wantErr: true},
		{name: "an unknown color scheme", opts: MediaEmulationOptions{ColorScheme: "sepia"}, wantErr: true},
		{name: "an unknown reduced-motion value", opts: MediaEmulationOptions{ReducedMotion: "off"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NormalizeMediaEmulation(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeMediaEmulation(%+v) error = %v, wantErr %v", tt.opts, err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeGeolocationOptions(t *testing.T) {
	lat, lng := 51.5007, -0.1246
	tooFarNorth := 91.0
	tests := []struct {
		name    string
		opts    GeolocationOptions
		want    GeolocationConfig
		wantErr bool
	}{
		{
			name: "accuracy defaults",
			opts: GeolocationOptions{Latitude: &lat, Longitude: &lng},
			want: GeolocationConfig{Latitude: lat, Longitude: lng, Accuracy: defaultGeolocationAccuracy},
		},
		{name: "longitude alone", opts: GeolocationOptions{Longitude: &lng}, wantErr: true},
		{name: "out of range", opts: GeolocationOptions{Latitude: &tooFarNorth, Longitude: &lng}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := NormalizeGeolocationOptions(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Clearing has to mean "no limit", not "a limit of zero bytes per second".
func TestNormalizeNetworkConditionsClearsToUnthrottled(t *testing.T) {
	cfg, clear, err := NormalizeNetworkConditions(NetworkConditionsOptions{Clear: true})
	if err != nil {
		t.Fatalf("NormalizeNetworkConditions: %v", err)
	}
	if !clear {
		t.Fatal("clear was not reported")
	}
	if cfg.Offline || cfg.LatencyMS != 0 || cfg.DownloadThroughput != unthrottled || cfg.UploadThroughput != unthrottled {
		t.Fatalf("cleared config = %+v, want online with no latency and no throughput cap", cfg)
	}
}
