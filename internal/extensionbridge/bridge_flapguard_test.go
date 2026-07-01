package extensionbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestExtensionFlapGuardRejectsColliding is the regression for the "flashing brw
// icon": two browser profiles running brw against ONE bridge endlessly displaced
// each other ("replaced by new extension connection" churn). Before the guard,
// EVERY new connection replaced the previous one (StatusNormalClosure) forever.
// After it, once a burst trips the flap the bridge HOLDS the live connection and
// REJECTS intruders (StatusTryAgainLater) — so at least one connection in a
// colliding burst is rejected rather than promoted.
func TestExtensionFlapGuardRejectsColliding(t *testing.T) {
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	var mu sync.Mutex
	rejected := 0 // connections closed with StatusTryAgainLater (guard fired)
	var conns []*websocket.Conn

	// Each connection gets a reader goroutine: draining keeps the HELD connection
	// alive (it answers keepalive pings) so subsequent intruders are genuinely
	// rejected rather than accepted onto a dead slot, and records the close code.
	for i := 0; i < flapThreshold+5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
		})
		cancel()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, c)
		go func(c *websocket.Conn) {
			for {
				if _, _, rerr := c.Read(context.Background()); rerr != nil {
					if websocket.CloseStatus(rerr) == websocket.StatusTryAgainLater {
						mu.Lock()
						rejected++
						mu.Unlock()
					}
					return
				}
			}
		}(c)
		time.Sleep(15 * time.Millisecond)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close(websocket.StatusNormalClosure, "test done")
		}
	}()

	time.Sleep(250 * time.Millisecond) // let rejections propagate to the readers

	mu.Lock()
	got := rejected
	mu.Unlock()
	if got == 0 {
		t.Fatalf("no connection was rejected with StatusTryAgainLater; the flap guard did not fire — colliding extensions would churn forever")
	}
	t.Logf("flap guard rejected %d colliding connection(s)", got)
}
