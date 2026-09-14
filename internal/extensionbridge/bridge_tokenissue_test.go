package extensionbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
