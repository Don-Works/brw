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
)

// dialExtension opens a websocket to the bridge's /extension endpoint with the
// given Origin header. Returns the connection and the dial error.
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

// TestHandshakeTokenAcceptedAndRejected proves that with a token configured, only
// a hello carrying the correct token brings the connection live.
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
		// Give the server time to process and reject; the connection must never
		// become the live bridge.
		time.Sleep(300 * time.Millisecond)
		if b.liveConn() {
			t.Fatal("connection with a wrong handshake token must not go live")
		}
	})

	t.Run("missing token goes live (graceful compat)", func(t *testing.T) {
		// Default (non-strict) mode keeps a not-yet-reloaded pre-0.2.0 extension
		// working: a hello with no token still connects.
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

// TestEmptyOriginRejected proves a connection with no Origin header (a non-browser
// local client) is refused at the upgrade, closing the coder/websocket gap.
func TestEmptyOriginRejected(t *testing.T) {
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	if conn, err := dialExtension(t, wsURL, ""); err == nil {
		conn.Close(websocket.StatusNormalClosure, "done")
		t.Fatal("a websocket with no Origin header must be rejected")
	}
	// A non-extension (web page) Origin must also be rejected.
	if conn, err := dialExtension(t, wsURL, "https://evil.com"); err == nil {
		conn.Close(websocket.StatusNormalClosure, "done")
		t.Fatal("a non-extension Origin must be rejected")
	}
}

// TestStatusTokenGating proves /status hands the token to a loopback,
// non-web-origin caller (the extension) and withholds it from a browser web
// origin or a non-loopback (DNS-rebinding) Host.
func TestStatusTokenGating(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetAuthToken("s3cret-token")

	statusToken := func(host, origin string) string {
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
		tok, _ := body["token"].(string)
		return tok
	}

	if got := statusToken("127.0.0.1:17311", ""); got != "s3cret-token" {
		t.Errorf("extension (loopback, no origin) should receive the token, got %q", got)
	}
	if got := statusToken("localhost:17311", "chrome-extension://abc"); got != "s3cret-token" {
		t.Errorf("extension origin over loopback should receive the token, got %q", got)
	}
	if got := statusToken("127.0.0.1:17311", "https://evil.com"); got != "" {
		t.Errorf("web origin must NOT receive the token, got %q", got)
	}
	if got := statusToken("evil.com:17311", ""); got != "" {
		t.Errorf("non-loopback (rebinding) Host must NOT receive the token, got %q", got)
	}
}
