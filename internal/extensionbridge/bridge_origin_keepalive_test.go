package extensionbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/coder/websocket"
)

const testDefaultOrigin = "chrome-extension://" + profilepolicy.DefaultBridgeExtensionID

func TestEffectiveExtensionID(t *testing.T) {

	explicit := New("", time.Second, "abcdefghijklmnopabcdefghijklmnop")
	if got := explicit.effectiveExtensionID(); got != "abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("configured effectiveExtensionID = %q, want the profile id", got)
	}

	unset := New("", time.Second, "")
	if got, want := unset.effectiveExtensionID(), strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID); got != want {
		t.Fatalf("unconfigured effectiveExtensionID = %q, want default %q", got, want)
	}
	if profilepolicy.DefaultBridgeExtensionID == "" && unset.effectiveExtensionID() != "" {
		t.Fatal("with no published default, the effective id must be empty (wildcard fallback)")
	}
}

func TestConfiguredExtensionOriginAcceptedAndOthersRejected(t *testing.T) {
	const id = "abcdefghijklmnopabcdefghijklmnop"
	b := New("", time.Second, id)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	okCtx, okCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer okCancel()
	conn, _, err := websocket.Dial(okCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://" + id}},
	})
	if err != nil {
		t.Fatalf("configured extension origin must be accepted: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	bad, _, err := websocket.Dial(badCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}},
	})
	if err == nil {
		_ = bad.Close(websocket.StatusNormalClosure, "should not have connected")
		t.Fatal("a non-configured extension origin must be rejected")
	}
}

func TestKeepAliveStopsWhenConnCloses(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {

		b.keepAlive(ctx, b.serverConn(), 5*time.Millisecond)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepAlive did not exit after context cancel; goroutine leak")
	}
}

func TestKeepAliveClosesConnOnDeadLink(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serverConn := b.serverConn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		b.keepAlive(ctx, serverConn, 5*time.Millisecond)
		close(done)
	}()

	_ = conn.CloseNow()

	select {
	case <-done:

	case <-time.After(3 * time.Second):
		t.Fatal("keepAlive did not exit after the link died")
	}
}

func TestKeepAliveExitsWhenConnReplaced(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	serverConn := b.serverConn()

	b.mu.Lock()
	b.conn = nil
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {

		b.keepAlive(ctx, serverConn, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replaced-conn keepAlive did not exit on its own when no longer the active conn")
	}
}

func (b *Bridge) serverConn() *websocket.Conn {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.conn
}
