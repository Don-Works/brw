package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// retargetFakeExtension is a minimal extension stand-in for the batch/plan
// retarget tests. It models a single authoritative foreground tab (backing both
// get_active_tab_id and list_tabs), accepts focus_tab to move it, and records
// the tabId of every cdp request so a test can prove which tab each page action
// targeted. Its cdp reply is a permissive superset that satisfies the bridge's
// ElementBox / ClickXYResult / ScrollResult parsers (all only require ok:true).
type retargetFakeExtension struct {
	mu          sync.Mutex
	foreground  int
	cdpTabIDs   []int
	focusedTabs []int
}

func (f *retargetFakeExtension) recordCDP(tabID int) {
	f.mu.Lock()
	f.cdpTabIDs = append(f.cdpTabIDs, tabID)
	f.mu.Unlock()
}

func (f *retargetFakeExtension) cdpTargets() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.cdpTabIDs))
	copy(out, f.cdpTabIDs)
	return out
}

func (f *retargetFakeExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		f.mu.Lock()
		var result any
		ok := true
		switch msg.Type {
		case "get_active_tab_id":
			result = map[string]any{"tabId": f.foreground}
		case "list_tabs":
			result = []map[string]any{{
				"id": f.foreground, "url": "https://x.test", "title": "X",
				"active": true, "windowId": 1, "windowFocused": true, "windowType": "normal",
			}}
		case "focus_tab":
			id := intParam(msg.Params["tabId"])
			f.foreground = id
			f.focusedTabs = append(f.focusedTabs, id)
			result = map[string]any{"id": id, "active": true, "windowId": 1, "windowType": "normal"}
		case "cdp":
			f.cdpTabIDs = append(f.cdpTabIDs, intParam(msg.Params["tabId"]))
			result = map[string]any{"result": map[string]any{"value": map[string]any{
				"ok": true, "viewportX": 1, "viewportY": 1, "x": 1, "y": 1, "width": 1, "height": 1,
				"target": "window", "name": "",
			}}}
		default:
			result = map[string]any{}
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": ok, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func intParam(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func connectRetargetFake(t *testing.T, b *Bridge, foreground int) (*retargetFakeExtension, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	fe := &retargetFakeExtension{foreground: foreground}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go fe.serve(serveCtx, conn)
	cleanup := func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
	return fe, cleanup
}

// TestBatchFocusTabRetargetsSubsequentSteps is the regression test for the
// ITEM C trap: a focus_tab step mid-batch legitimately changes the active tab,
// so every step AFTER it must target the newly-focused tab, not the tab pinned
// at the start of the batch. The fake records the tabId of every cdp request; a
// scroll before the focus_tab must hit tab 10, a scroll after it must hit tab 20.
func TestBatchFocusTabRetargetsSubsequentSteps(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectRetargetFake(t, b, 10)
	defer cleanup()

	res, err := b.ExecuteBatch(context.Background(), []browser.BatchStep{
		{Action: "scroll", Direction: "down"},
		{Action: "focus_tab", ID: "20"},
		{Action: "scroll", Direction: "down"},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if !res.OK {
		t.Fatalf("batch not OK: %+v", res)
	}

	// Each scroll step issues exactly one cdp evaluate; focus_tab uses its own
	// RPC (not cdp). The first cdp must target the pre-focus tab (10) and the cdp
	// after the focus_tab must target the newly-focused tab (20). A stale-pin bug
	// would keep targeting 10 after the focus.
	targets := fe.cdpTargets()
	if len(targets) < 2 {
		t.Fatalf("expected at least 2 cdp page actions, got %v", targets)
	}
	if targets[0] != 10 {
		t.Fatalf("first scroll targeted tab %d, want 10 (the pre-focus active tab)", targets[0])
	}
	if targets[len(targets)-1] != 20 {
		t.Fatalf("scroll after focus_tab targeted tab %d, want 20 (the newly-focused tab); a stale-pin bug would keep 10", targets[len(targets)-1])
	}
}

// TestPlanFocusTabRetargetsSubsequentSteps mirrors the batch test for the plan
// runner, which shares the same pin/retarget helpers.
func TestPlanFocusTabRetargetsSubsequentSteps(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectRetargetFake(t, b, 10)
	defer cleanup()

	res, err := b.ExecutePlan(context.Background(), []browser.PlanStep{
		{Action: "scroll", Direction: "down"},
		{Action: "focus_tab", ID: "20"},
		{Action: "scroll", Direction: "down"},
	})
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if !res.OK {
		t.Fatalf("plan not OK: %+v", res)
	}

	targets := fe.cdpTargets()
	if len(targets) < 2 {
		t.Fatalf("expected at least 2 cdp page actions, got %v", targets)
	}
	if targets[0] != 10 {
		t.Fatalf("first scroll targeted tab %d, want 10", targets[0])
	}
	if targets[len(targets)-1] != 20 {
		t.Fatalf("scroll after focus_tab targeted tab %d, want 20", targets[len(targets)-1])
	}
}

// TestBatchPinsActiveTabOnce proves the resolution multiplier is collapsed: a
// multi-step batch with NO focus_tab resolves the active tab a bounded number of
// times (one get_active_tab_id for the pin, not one per page sub-call). We count
// get_active_tab_id RPCs via a counting fake.
func TestBatchPinsActiveTabOnce(t *testing.T) {
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://fake"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})

	var mu sync.Mutex
	activeQueries := 0
	go func() {
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				ID     string         `json:"id"`
				Type   string         `json:"type"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			var result any = map[string]any{}
			switch msg.Type {
			case "get_active_tab_id":
				mu.Lock()
				activeQueries++
				mu.Unlock()
				result = map[string]any{"tabId": 10}
			case "cdp":
				result = map[string]any{"result": map[string]any{"value": map[string]any{"ok": true, "target": "window"}}}
			}
			reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
			_ = conn.Write(ctx, websocket.MessageText, reply)
		}
	}()

	_, err = b.ExecuteBatch(context.Background(), []browser.BatchStep{
		{Action: "scroll", Direction: "down"},
		{Action: "scroll", Direction: "down"},
		{Action: "scroll", Direction: "down"},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	mu.Lock()
	got := activeQueries
	mu.Unlock()
	// Without pinning, three scroll steps each re-resolve the active tab (≥3
	// get_active_tab_id). With the one-shot pin, the whole step loop resolves it
	// once; the final observation (obsCtx) re-resolves once more. Allow a small
	// bound to stay robust, but it must be far below the per-step multiplier.
	if got > 2 {
		t.Fatalf("active-tab resolved %d times across a 3-step batch; pin should collapse it to <=2", got)
	}
}
