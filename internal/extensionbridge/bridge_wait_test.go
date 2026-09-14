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

// serveRPCStub connects a fake extension that answers each RPC from reply, which
// is called with the message type and how many times that type has been asked.
// Returning ok=false sends the error form the bridge's capability checks read.
func serveRPCStub(t *testing.T, b *Bridge, reply func(msgType string, call int) (result map[string]any, ok bool, errMsg string)) func() {
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
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		var mu sync.Mutex
		calls := map[string]int{}
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			mu.Lock()
			call := calls[msg.Type]
			calls[msg.Type] = call + 1
			mu.Unlock()
			result, ok, errMsg := reply(msg.Type, call)
			out := map[string]any{"id": msg.ID, "ok": ok}
			if ok {
				out["result"] = result
			} else {
				out["error"] = errMsg
			}
			encoded, _ := json.Marshal(out)
			_ = conn.Write(serveCtx, websocket.MessageText, encoded)
		}
	}()
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// The extension transport has no debugger attached, so it cannot subscribe to
// Browser.downloadProgress or Page.javascriptDialogOpening. The same waits must
// still resolve, and must say plainly that they cost a re-ask per check.
func TestBridgeDownloadWaitResolvesByPolling(t *testing.T) {
	b := New("", 5*time.Second, "")
	inProgress := map[string]any{
		"guid": "7", "url": "https://example.test/ledger.csv",
		"suggested_filename": "ledger.csv", "state": "in_progress",
	}
	completed := map[string]any{
		"guid": "7", "url": "https://example.test/ledger.csv",
		"suggested_filename": "ledger.csv", "state": "completed",
	}
	// The download is still running for the first two reads, so the wait cannot
	// resolve on its baseline read and has to come back for more.
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		downloads := []map[string]any{inProgress}
		if call >= 2 {
			downloads = []map[string]any{completed}
		}
		return map[string]any{"supported": true, "downloads": downloads}, true, ""
	})
	defer cleanup()

	outcome, err := b.WaitForOutcome(context.Background(), "download:ledger", 5*time.Second)
	if err != nil {
		t.Fatalf("wait for download: %v", err)
	}
	if outcome.ResolvedBy != browser.WaitResolvedByPoll {
		t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, browser.WaitResolvedByPoll)
	}
	if outcome.Wakeups < 3 {
		t.Fatalf("wakeups = %d, want at least the 3 reads the fixture needed", outcome.Wakeups)
	}
}

// A download that is already finished on the wait's very first read is the
// baseline, not the answer: the same rule the direct-CDP wait applies, so the
// wait written after a click cannot be satisfied by an older file.
func TestBridgeDownloadWaitIgnoresDownloadsFinishedBeforeItStarted(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return map[string]any{"supported": true, "downloads": []map[string]any{{
			"guid": "old", "url": "https://example.test/last-week.pdf",
			"suggested_filename": "last-week.pdf", "state": "completed",
		}}}, true, ""
	})
	defer cleanup()

	_, err := b.WaitForOutcome(context.Background(), "download", 700*time.Millisecond)
	if err == nil {
		t.Fatal("a download that was already finished before the wait began must not satisfy it")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// A transport that cannot answer at all returns the NAMED capability error
// rather than silently timing out, so the caller learns why.
func TestBridgeDownloadWaitNamesTheMissingCapability(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return nil, false, "unknown message type get_downloads"
	})
	defer cleanup()

	_, err := b.WaitForOutcome(context.Background(), "download", 2*time.Second)
	if err == nil {
		t.Fatal("a wait the extension cannot answer must fail, not time out silently")
	}
	if !strings.Contains(err.Error(), "predates chrome.downloads support") {
		t.Fatalf("err = %v, want the named download capability error", err)
	}
}

func TestBridgeDialogWaitResolvesByPolling(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		if call < 2 {
			return map[string]any{"dialogs": []map[string]any{}}, true, ""
		}
		return map[string]any{"dialogs": []map[string]any{{
			"type": "confirm", "message": "Delete this fixture?",
			"accepted": false, "decided_by": "user_safe_default",
			"at": time.Now().UTC().Format(time.RFC3339Nano),
		}}}, true, ""
	})
	defer cleanup()

	outcome, err := b.WaitForOutcome(context.Background(), "dialog:delete this fixture", 5*time.Second)
	if err != nil {
		t.Fatalf("wait for dialog: %v", err)
	}
	if outcome.ResolvedBy != browser.WaitResolvedByPoll {
		t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, browser.WaitResolvedByPoll)
	}
}

// A dialog from an earlier step is outside the recency window and is not the one
// the caller's click just raised.
func TestBridgeDialogWaitIgnoresDialogsOutsideTheRecencyWindow(t *testing.T) {
	b := New("", 5*time.Second, "")
	stale := time.Now().Add(-browser.RecentDialogWindow - time.Minute).UTC().Format(time.RFC3339Nano)
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return map[string]any{"dialogs": []map[string]any{{
			"type": "alert", "message": "from an earlier step",
			"accepted": true, "decided_by": "agent_acting", "at": stale,
		}}}, true, ""
	})
	defer cleanup()

	_, err := b.WaitForOutcome(context.Background(), "dialog", 700*time.Millisecond)
	if err == nil {
		t.Fatal("a dialog from outside the recency window must not satisfy the wait")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// The polling cadence must back off rather than hammer the message port for the
// whole timeout: each check is one RPC over the bridge.
func TestBridgePollingCadenceBacksOff(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return map[string]any{"dialogs": []map[string]any{}}, true, ""
	})
	defer cleanup()

	outcome, _ := b.WaitForOutcome(context.Background(), "dialog", 2*time.Second)
	// At the 60 ms start with no backoff a 2 s wait would be ~33 reads; backing
	// off to a 400 ms ceiling keeps it near ten.
	if outcome.Wakeups > 14 {
		t.Fatalf("a 2s dialog wait made %d bridge reads — the cadence is not backing off", outcome.Wakeups)
	}
	if outcome.Wakeups < 2 {
		t.Fatalf("a 2s dialog wait made only %d reads — it is not checking at all", outcome.Wakeups)
	}
}
