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

func statusTokenFor(t *testing.T, b *Bridge, host, origin string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	b.handleStatus(rec, req)
	return tokenInBody(t, rec)
}

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

		{name: "a field the size of the read limit still lands at the limit", value: strings.Repeat("a", 4<<20), want: strings.Repeat("a", handshakeFieldLimit)},

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

func allocatedBytes(call func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	call()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

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

func TestStatusTokenCallerMatrixIsExhaustive(t *testing.T) {

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
