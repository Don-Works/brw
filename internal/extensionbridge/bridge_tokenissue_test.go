package extensionbridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

const (
	tokenIssueSecret   = "fixture-bridge-handshake-token-one"
	otherExtensionID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	configuredExtnID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenIssueLoopback = "127.0.0.1:17311"
)

// statusTokenFor asks /status for the handshake token with a given Host and
// Origin, exactly as a caller on the loopback interface would.
func statusTokenFor(t *testing.T, b *Bridge, host, origin string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	b.handleStatus(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	token, _ := body["token"].(string)
	return token
}

// TestStatusTokenOriginIsAnExactMatch: the guard was a PREFIX match on
// "chrome-extension://", so every string beginning with those characters passed
// — a prefix match on attacker-supplied input, the bug class wave 2 found
// elsewhere in this codebase. It is now an exact comparison against the
// configured extension id.
//
// This is hygiene, not a new boundary, and the last case says why: a caller that
// sends NO Origin is still served, because that is what the real extension's
// privileged loopback fetch looks like. What the token does and does not protect
// is argued in docs/auth-model.md.
func TestStatusTokenOriginIsAnExactMatch(t *testing.T) {
	tests := []struct {
		name         string
		configuredID string
		host         string
		origin       string
		want         string
	}{
		{
			name:         "the configured extension is served",
			configuredID: configuredExtnID,
			host:         tokenIssueLoopback,
			origin:       "chrome-extension://" + configuredExtnID,
			want:         tokenIssueSecret,
		},
		{
			name:         "a different extension id is refused",
			configuredID: configuredExtnID,
			host:         tokenIssueLoopback,
			origin:       "chrome-extension://" + otherExtensionID,
			want:         "",
		},
		{
			name: "an unconfigured bridge still pins to the published extension",
			// effectiveExtensionID falls back to the published id rather than a
			// wildcard, so "unconfigured" must not mean "any id".
			host:   tokenIssueLoopback,
			origin: "chrome-extension://" + otherExtensionID,
			want:   "",
		},
		{
			name:   "the published extension is served on an unconfigured bridge",
			host:   tokenIssueLoopback,
			origin: "chrome-extension://" + profilepolicy.DefaultBridgeExtensionID,
			want:   tokenIssueSecret,
		},
		{
			name:         "an origin that merely starts with the configured one is refused",
			configuredID: configuredExtnID,
			host:         tokenIssueLoopback,
			origin:       "chrome-extension://" + configuredExtnID + ".evil",
			want:         "",
		},
		{
			name:         "a web origin is refused",
			configuredID: configuredExtnID,
			host:         tokenIssueLoopback,
			origin:       "https://evil.example",
			want:         "",
		},
		{
			name:         "a rebinding Host is refused even with the right origin",
			configuredID: configuredExtnID,
			host:         "evil.example:17311",
			origin:       "chrome-extension://" + configuredExtnID,
			want:         "",
		},
		{
			// Measured on Chromium 152: an MV3 service worker fetching a loopback
			// URL it holds host_permissions for sends no Origin header. Refusing
			// this case would refuse the extension itself.
			name:         "a caller that sends no Origin is served, including one that is not the extension",
			configuredID: configuredExtnID,
			host:         tokenIssueLoopback,
			want:         tokenIssueSecret,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, tc.configuredID)
			b.SetAuthToken(tokenIssueSecret)
			if got := statusTokenFor(t, b, tc.host, tc.origin); got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRejectedHandshakeRecordsTheEndpointItTried: a hello with no token is the
// shape a stale bridge config produces — the extension could not reach its
// status URL, so it had nothing to present. The URL it tried lives only in the
// extension's chrome.storage.local, which nothing outside the browser can read,
// so the bridge has to keep what the rejected hello reported or `brwctl doctor`
// has no way to name the fault from this side.
func TestRejectedHandshakeRecordsTheEndpointItTried(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetAuthToken(tokenIssueSecret)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusPolicyViolation, "done")

	const deadStatusURL = "http://127.0.0.1:19999/status"
	msg, _ := json.Marshal(map[string]any{
		"type": "hello",
		"hello": map[string]any{
			"source":        "brw-extension",
			"status_url":    deadStatusURL,
			"bridge_url":    "ws://127.0.0.1:19999/extension",
			"config_source": "stored",
			// No token: the extension could not fetch one from a dead status URL.
		},
	})
	if err := conn.Write(t.Context(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	var last map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.Host = tokenIssueLoopback
		rec := httptest.NewRecorder()
		b.handleStatus(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if reported, ok := body["last_handshake"].(map[string]any); ok {
			last = reported
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last == nil {
		t.Fatal("/status published no last_handshake after a refused hello")
	}
	if got, _ := last["status_url"].(string); got != deadStatusURL {
		t.Fatalf("last_handshake.status_url = %q, want %q", got, deadStatusURL)
	}
	if got, _ := last["config_source"].(string); got != "stored" {
		t.Fatalf("last_handshake.config_source = %q, want %q", got, "stored")
	}
	if got, _ := last["reason"].(string); !strings.Contains(got, "missing handshake token") {
		t.Fatalf("last_handshake.reason = %q, want the missing-token rejection", got)
	}
}

// TestHandshakeReportIsSanitized: what a refused handshake reports arrives on an
// unauthenticated connection and ends up printed to an operator's terminal by
// `brwctl doctor`, so a rogue local client must not get to choose the escape
// sequences or the length.
func TestHandshakeReportIsSanitized(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "an ordinary URL is untouched", value: "http://127.0.0.1:17311/status", want: "http://127.0.0.1:17311/status"},
		{name: "terminal escapes are stripped", value: "http://host/\x1b[2Jcleared\x07", want: "http://host/[2Jcleared"},
		{name: "newlines cannot forge a second report line", value: "http://a\nstatus_url: http://b", want: "http://astatus_url: http://b"},
		{name: "a long field is truncated", value: strings.Repeat("a", handshakeFieldLimit+50), want: strings.Repeat("a", handshakeFieldLimit)},
		{name: "truncation does not split a rune", value: strings.Repeat("é", handshakeFieldLimit+10), want: strings.Repeat("é", handshakeFieldLimit)},
		{name: "empty stays empty", value: "", want: ""},
		// The websocket read limit is 4 MiB and this runs before the client has
		// presented anything, so the whole value must never be mapped or turned
		// into a []rune: that is a multi-megabyte copy plus one four times the
		// size, per field, per refused handshake, chosen by the caller.
		{name: "a field the size of the read limit still lands at the limit", value: strings.Repeat("a", 4<<20), want: strings.Repeat("a", handshakeFieldLimit)},
		// 3-byte runes put the byte cut mid-character, which must not leave a
		// replacement character where the URL used to be.
		{name: "the byte cut does not split a rune either", value: strings.Repeat("€", 4<<18), want: strings.Repeat("€", handshakeFieldLimit)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeHandshakeField(tc.value); got != tc.want {
				t.Fatalf("sanitizeHandshakeField(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestRefusedHelloNeverEchoesTheToken guards the path opened by returning the
// rejected hello: a wrong token presented on a refused handshake must not come
// back out of /status, which is readable by exactly the caller that guessed.
func TestRefusedHelloNeverEchoesTheToken(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetAuthToken(tokenIssueSecret)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusPolicyViolation, "done")

	const guessed = "fixture-wrong-handshake-token-two"
	sendHello(t, conn, guessed)

	deadline := time.Now().Add(3 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.Host = tokenIssueLoopback
		rec := httptest.NewRecorder()
		b.handleStatus(rec, req)
		body := rec.Body.String()
		if strings.Contains(body, guessed) {
			t.Fatalf("/status echoed the token a refused hello presented: %s", body)
		}
		if strings.Contains(body, "last_handshake") || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSanitizeHandshakeFieldIsBoundedBeforeItMaps: the byte cut is the part with
// a cost attached, so assert on the work rather than only the result — a value
// at the websocket read limit must not be walked rune by rune to produce 256
// runes, and whatever survives must still be valid UTF-8.
func TestSanitizeHandshakeFieldIsBoundedBeforeItMaps(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "ascii at the read limit", value: strings.Repeat("a", 4<<20)},
		{name: "multi-byte runes at the read limit", value: strings.Repeat("€", 4<<18)},
		{name: "control characters that map away, then a split rune", value: strings.Repeat("\x01", handshakeFieldLimit*utf8.UTFMax-1) + "€"},
		{name: "trailing bytes that never decoded", value: strings.Repeat("a", handshakeFieldLimit*utf8.UTFMax) + "\xff\xff\xff"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			// The cost is the assertion, not just the result: a value that is
			// mapped and rune-converted before it is truncated allocates the
			// whole input plus a []rune four times its size. At 4 MiB that is
			// ~16 MiB the caller chose, so a megabyte bound fails on that and
			// passes with three orders of magnitude to spare on 256 runes.
			used := allocatedBytes(func() { got = sanitizeHandshakeField(tc.value) })
			if used > 1<<20 {
				t.Fatalf("sanitizeHandshakeField allocated %d bytes for a %d-byte field cut to %d runes",
					used, len(tc.value), handshakeFieldLimit)
			}
			if n := utf8.RuneCountInString(got); n > handshakeFieldLimit {
				t.Fatalf("sanitizeHandshakeField kept %d runes, want at most %d", n, handshakeFieldLimit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("sanitizeHandshakeField returned invalid UTF-8: %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("the byte cut left a replacement character: %q", got)
			}
		})
	}
}

// allocatedBytes reports what one call added to the process total. TotalAlloc is
// cumulative and unaffected by collection, so this measures the work done rather
// than what survived it.
func allocatedBytes(call func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	call()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestRefusedHandshakeReasonIsSanitized: the reason is the one recorded field
// built from an error rather than from a fixed string, and it is published
// TWICE — as last_handshake.reason, and as disconnect_reason, which `brwctl
// doctor` prints on its bridge_connected line. The error paths that can reach
// here today quote their input, but the record exists precisely because this
// connection has presented nothing, so the field that is one wrapped
// json.Unmarshal away from carrying the caller's bytes cannot be the raw one.
func TestRefusedHandshakeReasonIsSanitized(t *testing.T) {
	const hostile = "invalid hello frame: \x1b[2Jcleared\r\ndisconnect_reason: all good"

	b := New("", 5*time.Second, "")
	b.SetAuthToken(tokenIssueSecret)
	b.recordHandshakeRejection(hello{}, errors.New(hostile))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Host = tokenIssueLoopback
	rec := httptest.NewRecorder()
	b.handleStatus(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	last, ok := body["last_handshake"].(map[string]any)
	if !ok {
		t.Fatalf("/status published no last_handshake: %s", rec.Body.String())
	}
	reason, _ := last["reason"].(string)
	disconnect, _ := body["disconnect_reason"].(string)

	for _, field := range []struct{ name, value string }{
		{"last_handshake.reason", reason},
		{"disconnect_reason", disconnect},
	} {
		if field.value == "" {
			t.Fatalf("%s is empty; the refusal says nothing", field.name)
		}
		if strings.ContainsAny(field.value, "\x1b\r\n") {
			t.Fatalf("%s carries control characters an operator's terminal would act on: %q", field.name, field.value)
		}
		if !strings.Contains(field.value, "invalid hello frame") {
			t.Fatalf("%s lost the reason it was recording: %q", field.name, field.value)
		}
	}
}

// tokenCallerHosts and tokenCallerOrigins enumerate every shape of caller that
// can reach /status. The cross product below must have a verdict for each: a new
// Origin class added without one fails the test rather than inheriting whatever
// the last condition in tokenServable happens to do.
var (
	tokenCallerHosts = []struct{ name, host string }{
		{"loopback", tokenIssueLoopback},
		{"loopback by name", "localhost:17311"},
		{"a rebinding host", "evil.example:17311"},
	}
	tokenCallerOrigins = []struct{ name, origin string }{
		{"no Origin at all", ""},
		{"the configured extension", "chrome-extension://" + configuredExtnID},
		{"another extension", "chrome-extension://" + otherExtensionID},
		{"an id the configured one is a prefix of", "chrome-extension://" + configuredExtnID + ".evil"},
		{"a web page", "https://evil.example"},
		{"the null origin", "null"},
		{"an extension scheme with no id", "chrome-extension://"},
	}
)

// TestStatusTokenCallerMatrixIsExhaustive states who gets the token, for every
// caller shape rather than for the three that were interesting when the guard
// was written.
//
// The two true rows on loopback are the boundary, and the second of them is the
// one to read carefully: a caller that sends NO Origin is served, because that
// is what the real extension's privileged loopback fetch looks like (measured on
// Chromium 152: an MV3 service worker fetching a URL it holds host_permissions
// for sends no Origin and Sec-Fetch-Site: none). Requiring the header would
// refuse the only client this endpoint exists for. So any local process gets the
// token by simply omitting a header, and no property of the request separates it
// from the extension — every property of a request is chosen by whoever sends
// it. docs/auth-model.md argues that boundary instead of pretending this
// function moves it.
func TestStatusTokenCallerMatrixIsExhaustive(t *testing.T) {
	// key is "<host>/<origin>"; every combination of the two lists above must
	// appear exactly once.
	served := map[string]bool{
		"loopback/no Origin at all":                        true,
		"loopback/the configured extension":                true,
		"loopback/another extension":                       false,
		"loopback/an id the configured one is a prefix of": false,
		"loopback/a web page":                              false,
		"loopback/the null origin":                         false,
		"loopback/an extension scheme with no id":          false,

		"loopback by name/no Origin at all":                        true,
		"loopback by name/the configured extension":                true,
		"loopback by name/another extension":                       false,
		"loopback by name/an id the configured one is a prefix of": false,
		"loopback by name/a web page":                              false,
		"loopback by name/the null origin":                         false,
		"loopback by name/an extension scheme with no id":          false,

		// A DNS-rebinding page reaches the daemon with an attacker Host and no
		// Origin. The Host check is the guard that does work no other guard does.
		"a rebinding host/no Origin at all":                        false,
		"a rebinding host/the configured extension":                false,
		"a rebinding host/another extension":                       false,
		"a rebinding host/an id the configured one is a prefix of": false,
		"a rebinding host/a web page":                              false,
		"a rebinding host/the null origin":                         false,
		"a rebinding host/an extension scheme with no id":          false,
	}

	seen := map[string]bool{}
	for _, host := range tokenCallerHosts {
		for _, origin := range tokenCallerOrigins {
			key := host.name + "/" + origin.name
			want, listed := served[key]
			if !listed {
				t.Fatalf("caller shape %q has no verdict: add it to served, deciding deliberately whether it may have the token", key)
			}
			seen[key] = true
			t.Run(key, func(t *testing.T) {
				b := New("", 5*time.Second, configuredExtnID)
				b.SetAuthToken(tokenIssueSecret)
				got := statusTokenFor(t, b, host.host, origin.origin) != ""
				if got != want {
					t.Fatalf("token served = %v, want %v", got, want)
				}
			})
		}
	}
	for key := range served {
		if !seen[key] {
			t.Fatalf("served lists %q, which is no longer a caller shape this matrix produces", key)
		}
	}
}
