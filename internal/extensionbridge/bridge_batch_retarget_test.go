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

type retargetFakeExtension struct {
	mu          sync.Mutex
	foreground  int
	cdpTabIDs   []int
	focusedTabs []int
}

func (f *retargetFakeExtension) cdpTargets() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.cdpTabIDs))
	copy(out, f.cdpTabIDs)
	return out
}

func TestRetargetPinnedTabDistinguishesImplicitLeaseFromExplicitCallerPin(t *testing.T) {
	b := &Bridge{}
	implicit := browser.WithImplicitTabID(context.Background(), "41")
	got := b.retargetPinnedTab(implicit, implicit, "42")
	if tabID := browser.TabIDFromContext(got); tabID != "42" {
		t.Fatalf("implicit lease retarget = %q, want 42", tabID)
	}
	if browser.TabIDIsExplicit(got) {
		t.Fatal("retargeted implicit lease became an explicit caller pin")
	}
	if browser.TabIDRequiresCurrentOwnership(got) {
		t.Fatal("retargeted session lease became a single-global-tab ownership pin")
	}

	owned := browser.WithCurrentOwnedTabID(context.Background(), "45")
	got = b.retargetPinnedTab(context.Background(), owned, "46")
	if tabID := browser.TabIDFromContext(got); tabID != "46" {
		t.Fatalf("current-owned pin retarget = %q, want 46", tabID)
	}
	if !browser.TabIDRequiresCurrentOwnership(got) {
		t.Fatal("retargeted current-owned pin lost reconnect validation marker")
	}

	explicit := browser.WithTabID(context.Background(), "51")
	got = b.retargetPinnedTab(explicit, explicit, "52")
	if tabID := browser.TabIDFromContext(got); tabID != "51" {
		t.Fatalf("explicit pin retarget = %q, want sticky 51", tabID)
	}
}

func TestOpenedChildTabIDIgnoresConcurrentUnrelatedOpen(t *testing.T) {
	tabs := []browser.Tab{
		{ID: "41"},
		{ID: "60", OpenerTabID: "59"},
		{ID: "42", OpenerTabID: "41"},
	}
	if got := openedChildTabID(tabs, map[string]bool{"41": true}, "41"); got != "42" {
		t.Fatalf("opened child = %q, want 42", got)
	}
	if got := openedChildTabID([]browser.Tab{{ID: "60", OpenerTabID: "59"}}, map[string]bool{}, "41"); got != "" {
		t.Fatalf("unrelated new tab was attributed to source: %q", got)
	}
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
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	conn.SetReadLimit(4 << 20)
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

func TestBatchPinsActiveTabOnce(t *testing.T) {
	b := New("", 5*time.Second, "")
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
	conn.SetReadLimit(4 << 20)
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

	if got > 2 {
		t.Fatalf("active-tab resolved %d times across a 3-step batch; pin should collapse it to <=2", got)
	}
}

func TestBatchExplicitTabIDStaysStickyAcrossFocusTab(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe, cleanup := connectRetargetFake(t, b, 10)
	defer cleanup()

	ctx := browser.WithTabID(context.Background(), "99")
	res, err := b.ExecuteBatch(ctx, []browser.BatchStep{
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
	targets := fe.cdpTargets()
	if len(targets) < 2 {
		t.Fatalf("expected at least 2 cdp page actions, got %v", targets)
	}
	for i, target := range targets {
		if target != 99 {
			t.Fatalf("cdp action %d targeted tab %d, want the explicit tab 99 — a focus_tab step must not override an explicit tab_id", i, target)
		}
	}
}
