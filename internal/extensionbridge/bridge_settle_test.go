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

func TestSettleReturnsEarlyOnQuiescentPage(t *testing.T) {
	b := New("", 5*time.Second, "")

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

func TestSettleNeverExceedsCap(t *testing.T) {
	b := New("", 5*time.Second, "")

	cleanup := connectSettleFakeChurning(t, b)
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")
	start := time.Now()
	b.settle(ctx, observedActionSettle)
	elapsed := time.Since(start)

	if elapsed > observedActionSettle+150*time.Millisecond {
		t.Fatalf("settle took %v; must stay bounded near the %v cap", elapsed, observedActionSettle)
	}
}

func TestSettleHonoursMinimumFloor(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := connectSettleFake(t, b, "complete|10|100|BODY#|https://x.test")
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "5")

	roundTripStart := time.Now()
	var warm string
	_ = b.evaluate(ctx, settleFingerprintExpr, "", &warm)
	roundTrip := time.Since(roundTripStart)

	start := time.Now()
	b.settle(ctx, observedActionSettle)
	elapsed := time.Since(start)

	if elapsed < settleMinFloor-5*time.Millisecond {
		t.Fatalf("settle returned in %v, below the %v floor; a delayed mutation could be missed", elapsed, settleMinFloor)
	}

	budget := settleMinFloor + 4*roundTrip
	if budget >= observedActionSettle {
		t.Skipf("transport round trip is %v, so floor+polls (%v) cannot be distinguished from the %v cap on this machine; floor assertion still held at %v",
			roundTrip, budget, observedActionSettle, elapsed)
	}
	if elapsed >= observedActionSettle {
		t.Fatalf("settle took %v (round trip %v); floor must not push it to the %v cap on a quiescent page", elapsed, roundTrip, observedActionSettle)
	}
}

func TestWaitForReturnsPromptlyWhenSatisfied(t *testing.T) {
	b := New("", 5*time.Second, "")

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

func TestWaitForRespectsCancellation(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := connectSettleFake(t, b, false)
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
