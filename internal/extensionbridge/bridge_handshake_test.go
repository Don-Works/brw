package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func dialExtension(t *testing.T, wsURL, origin string) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": []string{origin}}
	}
	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	return conn, err
}

func sendHello(t *testing.T, conn *websocket.Conn, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, _ := json.Marshal(map[string]any{
		"type":  "hello",
		"hello": map[string]any{"source": "brw-extension", "token": token},
	})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("write hello: %v", err)
	}
}

func (b *Bridge) liveConn() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.conn != nil
}

func TestHandshakeTokenAcceptedAndRejected(t *testing.T) {
	t.Run("valid token goes live", func(t *testing.T) {
		b := New("", 5*time.Second, "")
		b.SetAuthToken("s3cret-token")
		srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

		conn, err := dialExtension(t, wsURL, testDefaultOrigin)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		sendHello(t, conn, "s3cret-token")
		waitUntil(t, b.liveConn)
	})

	t.Run("wrong token never goes live", func(t *testing.T) {
		b := New("", 5*time.Second, "")
		b.SetAuthToken("s3cret-token")
		srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

		conn, err := dialExtension(t, wsURL, testDefaultOrigin)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusPolicyViolation, "done")
		sendHello(t, conn, "wrong-token")

		time.Sleep(300 * time.Millisecond)
		if b.liveConn() {
			t.Fatal("connection with a wrong handshake token must not go live")
		}
	})

	t.Run("missing token rejected by default", func(t *testing.T) {

		b := New("", 5*time.Second, "")
		b.SetAuthToken("s3cret-token")
		srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

		conn, err := dialExtension(t, wsURL, testDefaultOrigin)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusPolicyViolation, "done")
		sendHello(t, conn, "")
		time.Sleep(300 * time.Millisecond)
		if b.liveConn() {
			t.Fatal("a tokenless hello must be rejected without an explicit opt-out")
		}
	})

	t.Run("missing token accepted only when explicitly opted out", func(t *testing.T) {
		b := New("", 5*time.Second, "")
		b.SetAuthToken("s3cret-token")
		b.SetRequireToken(false)
		srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

		conn, err := dialExtension(t, wsURL, testDefaultOrigin)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		sendHello(t, conn, "")
		waitUntil(t, b.liveConn)
	})

	t.Run("missing token rejected in strict mode", func(t *testing.T) {
		b := New("", 5*time.Second, "")
		b.SetAuthToken("s3cret-token")
		b.SetRequireToken(true)
		srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

		conn, err := dialExtension(t, wsURL, testDefaultOrigin)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusPolicyViolation, "done")
		sendHello(t, conn, "")
		time.Sleep(300 * time.Millisecond)
		if b.liveConn() {
			t.Fatal("strict mode must reject a connection with no handshake token")
		}
	})
}

func TestRejectedHandshakeIsVisibleOnStatus(t *testing.T) {
	cases := []struct {
		name      string
		strict    bool
		presented string
		wantIn    string
		wantSeen  bool
	}{
		{name: "a wrong token", presented: "wrong-token", wantIn: "invalid handshake token", wantSeen: true},
		{name: "no token in strict mode", strict: true, wantIn: "missing handshake token", wantSeen: true},
		{name: "an accepted token records nothing", presented: "s3cret-token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			b.SetAuthToken("s3cret-token")
			b.SetRequireToken(tc.strict)
			srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
			defer srv.Close()
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

			conn, err := dialExtension(t, wsURL, testDefaultOrigin)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")
			sendHello(t, conn, tc.presented)
			if !tc.wantSeen {
				waitUntil(t, b.liveConn)
			}

			var reason string
			deadline := time.Now().Add(3 * time.Second)
			for {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/status", nil)
				req.Host = "127.0.0.1:17311"
				b.handleStatus(rec, req)
				var status map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
					t.Fatalf("decode status: %v", err)
				}
				reason, _ = status["disconnect_reason"].(string)
				if !tc.wantSeen || strings.Contains(reason, "handshake") || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}

			if !tc.wantSeen {
				if reason != "" {
					t.Fatalf("an accepted handshake recorded disconnect_reason %q", reason)
				}
				return
			}
			if !strings.Contains(reason, "handshake rejected") || !strings.Contains(reason, tc.wantIn) {
				t.Fatalf("disconnect_reason = %q, want the handshake rejection naming %q", reason, tc.wantIn)
			}
		})
	}
}

func TestEmptyOriginRejected(t *testing.T) {
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	if conn, err := dialExtension(t, wsURL, ""); err == nil {
		conn.Close(websocket.StatusNormalClosure, "done")
		t.Fatal("a websocket with no Origin header must be rejected")
	}

	if conn, err := dialExtension(t, wsURL, "https://evil.com"); err == nil {
		conn.Close(websocket.StatusNormalClosure, "done")
		t.Fatal("a non-extension Origin must be rejected")
	}
}

func TestStatusTokenGating(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetAuthToken("s3cret-token")

	if got := statusTokenFor(t, b, "127.0.0.1:17311", ""); got != "s3cret-token" {
		t.Errorf("extension (loopback, no origin) should receive the token, got %q", got)
	}
	if got := statusTokenFor(t, b, "localhost:17311", "chrome-extension://"+profilepolicy.DefaultBridgeExtensionID); got != "s3cret-token" {
		t.Errorf("the configured extension's origin over loopback should receive the token, got %q", got)
	}
	if got := statusTokenFor(t, b, "127.0.0.1:17311", "https://evil.com"); got != "" {
		t.Errorf("web origin must NOT receive the token, got %q", got)
	}
	if got := statusTokenFor(t, b, "evil.com:17311", ""); got != "" {
		t.Errorf("non-loopback (rebinding) Host must NOT receive the token, got %q", got)
	}
}
