package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// settleFake serves cdp Runtime.evaluate replies for the adaptive settle /
// WaitFor tests. value is the JSON value returned for every evaluate, so a test
// can serve a stable "complete" fingerprint (settle returns early) or a truthy
// condition (WaitFor returns early).
type settleFake struct {
	value any
}

func (f *settleFake) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		var result any = map[string]any{}
		if msg.Type == "cdp" {
			result = map[string]any{"result": map[string]any{"value": f.value}}
		}
		if msg.Type == "get_active_tab_id" {
			result = map[string]any{"tabId": 5}
		}
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectSettleFake(t *testing.T, b *Bridge, value any) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	fe := &settleFake{value: value}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fe.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "done")
		srv.Close()
	}
}

// connectSettleFakeChurning serves an ever-changing string fingerprint so the
// adaptive settle never sees two equal consecutive reads (worst case).
func connectSettleFakeChurning(t *testing.T, b *Bridge) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		n := 0
		for {
			_, data, err := conn.Read(serveCtx)
			if err != nil {
				return
			}
			var msg struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			n++
			var result any = map[string]any{}
			if msg.Type == "cdp" {
				result = map[string]any{"result": map[string]any{"value": "complete|" + strconv.Itoa(n) + "|x|y|z"}}
			}
			reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
			_ = conn.Write(serveCtx, websocket.MessageText, reply)
		}
	}()
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "done")
		srv.Close()
	}
}

// TestSettleReturnsEarlyOnQuiescentPage proves the adaptive settle returns well
// before the cap when the page fingerprint is already stable and complete,
// rather than always blocking the full fixed cap as the old time.Sleep did.
func TestSettleReturnsEarlyOnQuiescentPage(t *testing.T) {
	b := New("", 5*time.Second, "")
	// A stable "complete" fingerprint: every read returns the same value, so
	// settle reaches settleStableReads quickly and returns.
	cleanup := connectSettleFake(t, b, "complete|10|100|BODY#|https://x.test")
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")
	start := time.Now()
	b.settle(ctx, observedActionSettle)
	elapsed := time.Since(start)
	if elapsed >= observedActionSettle {
		t.Fatalf("settle took %v on a quiescent page; should return before the %v cap", elapsed, observedActionSettle)
	}
}

// TestSettleNeverExceedsCap guards the "NEVER slower than today" contract even
// when the page keeps mutating (fingerprint differs each read so it never
// reaches the stable threshold) — settle must still return within the cap (plus
// a small scheduling slack), never blocking longer than the old fixed sleep did.
func TestSettleNeverExceedsCap(t *testing.T) {
	b := New("", 5*time.Second, "")
	// A churning page: the fake flips the fingerprint each read so settle never
	// sees two equal reads. value is a counter object the evaluate parser can read
	// as a string only if it's a string, so we serve an ever-changing string by
	// embedding time — simplest is to serve a value that unmarshals but differs.
	cleanup := connectSettleFakeChurning(t, b)
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")
	start := time.Now()
	b.settle(ctx, observedActionSettle)
	elapsed := time.Since(start)
	// Allow generous slack for the websocket round trips + scheduler, but it must
	// be bounded near the cap, not unbounded.
	if elapsed > observedActionSettle+150*time.Millisecond {
		t.Fatalf("settle took %v; must stay bounded near the %v cap", elapsed, observedActionSettle)
	}
}

// TestSettleHonoursMinimumFloor proves settle does not return instantly on a
// page that already looks stable: it holds for at least settleMinFloor so a
// delayed handler (setTimeout(0) / framework render / rAF landing a few ms after
// the action) is still observed, preserving the debounce the old fixed sleep had.
func TestSettleHonoursMinimumFloor(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := connectSettleFake(t, b, "complete|10|100|BODY#|https://x.test")
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")
	start := time.Now()
	b.settle(ctx, observedActionSettle)
	elapsed := time.Since(start)
	if elapsed < settleMinFloor-5*time.Millisecond {
		t.Fatalf("settle returned in %v, below the %v floor; a delayed mutation could be missed", elapsed, settleMinFloor)
	}
	if elapsed >= observedActionSettle {
		t.Fatalf("settle took %v; floor must not push it to the %v cap on a quiescent page", elapsed, observedActionSettle)
	}
}

// TestWaitForReturnsPromptlyWhenSatisfied proves the tightened WaitFor returns
// well under the old coarse 250ms poll when the condition is immediately true.
func TestWaitForReturnsPromptlyWhenSatisfied(t *testing.T) {
	b := New("", 5*time.Second, "")
	// condition() evaluates an in-page boolean; serve true so the first poll
	// satisfies the wait.
	cleanup := connectSettleFake(t, b, true)
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")
	start := time.Now()
	if err := b.WaitFor(ctx, "ready", 5*time.Second); err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= waitForPollInterval {
		t.Fatalf("WaitFor took %v for an already-satisfied condition; the tightened poll should return well under the old %v", elapsed, waitForPollInterval)
	}
}

// TestWaitForRespectsCancellation guards that the tightened poll loop still
// honours context cancellation promptly.
func TestWaitForRespectsCancellation(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := connectSettleFake(t, b, false) // condition never satisfied
	defer cleanup()

	ctx, cancel := context.WithCancel(browser.WithTabID(context.Background(), "5"))
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := b.WaitFor(ctx, "ready", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("WaitFor after cancel = %v, want a cancelled error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("WaitFor did not honour cancellation promptly: %v", time.Since(start))
	}
}
