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

type rpcStub struct {
	mu    sync.Mutex
	calls map[string]int
	at    map[string][]time.Time
	stop  func()
}

func (s *rpcStub) record(msgType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.calls[msgType]
	s.calls[msgType] = call + 1
	s.at[msgType] = append(s.at[msgType], time.Now())
	return call
}

func (s *rpcStub) count(msgType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[msgType]
}

func (s *rpcStub) gaps(msgType string) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamps := s.at[msgType]
	gaps := make([]time.Duration, 0, len(stamps))
	for i := 1; i < len(stamps); i++ {
		gaps = append(gaps, stamps[i].Sub(stamps[i-1]))
	}
	return gaps
}

func serveRPCStub(t *testing.T, b *Bridge, reply func(msgType string, call int) (result map[string]any, ok bool, errMsg string)) *rpcStub {
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
	stub := &rpcStub{calls: map[string]int{}, at: map[string][]time.Time{}}
	stub.stop = func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
	go func() {
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
			result, ok, errMsg := reply(msg.Type, stub.record(msg.Type))
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
	return stub
}

func downloadEntry(state string, changedAt time.Time) map[string]any {
	entry := map[string]any{
		"guid": "7", "url": "https://example.test/ledger.csv",
		"suggested_filename": "ledger.csv", "state": state,
	}
	if !changedAt.IsZero() {
		entry["changed_at_ms"] = changedAt.UnixMilli()
	}
	return entry
}

func TestBridgeDownloadWaitResolvesByPolling(t *testing.T) {
	b := New("", 5*time.Second, "")

	stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		state := "in_progress"
		if call >= 2 {
			state = "completed"
		}
		return map[string]any{"supported": true, "downloads": []map[string]any{downloadEntry(state, time.Now())}}, true, ""
	})
	defer stub.stop()

	outcome, err := b.WaitForOutcome(context.Background(), "download:ledger", 5*time.Second)
	if err != nil {
		t.Fatalf("wait for download: %v", err)
	}
	if outcome.ResolvedBy != browser.WaitResolvedByPoll {
		t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, browser.WaitResolvedByPoll)
	}
	if got := stub.count("get_downloads"); got < 3 {
		t.Fatalf("the extension served %d get_downloads calls, want the 3 reads the fixture needed", got)
	}
	if outcome.Wakeups != stub.count("get_downloads") {
		t.Fatalf("wakeups = %d but the extension served %d get_downloads calls; wakeups must count the reads",
			outcome.Wakeups, stub.count("get_downloads"))
	}
}

func TestBridgeDownloadWaitAppliesTheRecencyWindow(t *testing.T) {
	tests := []struct {
		name      string
		changedAt time.Time
		wantErr   string
	}{
		{
			name:      "a download that finished moments ago satisfies the wait",
			changedAt: time.Now(),
		},
		{
			name:      "a download from earlier in the session does not",
			changedAt: time.Now().Add(-browser.RecentDownloadWindow - time.Minute),
			wantErr:   "timed out",
		},
		{
			name:    "an extension that reports no change time is treated as old news",
			wantErr: "timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
				return map[string]any{"supported": true, "downloads": []map[string]any{
					downloadEntry("completed", tt.changedAt),
				}}, true, ""
			})
			defer stub.stop()

			outcome, err := b.WaitForOutcome(context.Background(), "download", 700*time.Millisecond)

			if got := stub.count("get_downloads"); got == 0 {
				t.Fatal("the wait never read the extension's download registry")
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("wait for download: %v", err)
				}
				if outcome.ResolvedBy != browser.WaitResolvedByPoll {
					t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, browser.WaitResolvedByPoll)
				}
				if outcome.Wakeups != 1 {
					t.Fatalf("wakeups = %d, want 1: the download was already there on the first read", outcome.Wakeups)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got outcome %+v", tt.wantErr, outcome)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got := stub.count("get_downloads"); got < 2 {
				t.Fatalf("the extension served %d get_downloads calls, want the wait to keep re-asking", got)
			}
		})
	}
}

func TestBridgeWaitNamesTheMissingCapability(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		rpc       string
		wantErr   string
	}{
		{
			name:      "downloads",
			condition: "download",
			rpc:       "get_downloads",
			wantErr:   "predates chrome.downloads support",
		},
		{
			name:      "dialogs",
			condition: "dialog",
			rpc:       "get_dialogs",
			wantErr:   "predates brw_dialog support",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
				return nil, false, "unknown message type " + tt.rpc
			})
			defer stub.stop()

			_, err := b.WaitForOutcome(context.Background(), tt.condition, 2*time.Second)
			if err == nil {
				t.Fatal("a wait the extension cannot answer must fail, not time out silently")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want the named capability error %q", err, tt.wantErr)
			}
			if got := stub.count(tt.rpc); got == 0 {
				t.Fatalf("the wait never attempted %s, so it cannot have learned the capability is missing", tt.rpc)
			}
		})
	}
}

func TestBridgeDialogWaitResolvesByPolling(t *testing.T) {
	b := New("", 5*time.Second, "")
	stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		if call < 2 {
			return map[string]any{"dialogs": []map[string]any{}}, true, ""
		}
		return map[string]any{"dialogs": []map[string]any{{
			"type": "confirm", "message": "Delete this fixture?",
			"accepted": false, "decided_by": "user_safe_default",
			"at": time.Now().UTC().Format(time.RFC3339Nano),
		}}}, true, ""
	})
	defer stub.stop()

	outcome, err := b.WaitForOutcome(context.Background(), "dialog:delete this fixture", 5*time.Second)
	if err != nil {
		t.Fatalf("wait for dialog: %v", err)
	}
	if outcome.ResolvedBy != browser.WaitResolvedByPoll {
		t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, browser.WaitResolvedByPoll)
	}
	if got := stub.count("get_dialogs"); got < 3 {
		t.Fatalf("the extension served %d get_dialogs calls, want the 3 reads the fixture needed", got)
	}
}

func TestBridgeDialogWaitIgnoresDialogsOutsideTheRecencyWindow(t *testing.T) {
	b := New("", 5*time.Second, "")
	stale := time.Now().Add(-browser.RecentDialogWindow - time.Minute).UTC().Format(time.RFC3339Nano)
	stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return map[string]any{"dialogs": []map[string]any{{
			"type": "alert", "message": "from an earlier step",
			"accepted": true, "decided_by": "agent_acting", "at": stale,
		}}}, true, ""
	})
	defer stub.stop()

	_, err := b.WaitForOutcome(context.Background(), "dialog", 700*time.Millisecond)
	if err == nil {
		t.Fatal("a dialog from outside the recency window must not satisfy the wait")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}

	if got := stub.count("get_dialogs"); got < 2 {
		t.Fatalf("the extension served %d get_dialogs calls, want the wait to keep re-reading the dialog ring", got)
	}
}

func TestBridgePollingCadenceBacksOff(t *testing.T) {
	b := New("", 5*time.Second, "")
	stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		return map[string]any{"dialogs": []map[string]any{}}, true, ""
	})
	defer stub.stop()

	outcome, _ := b.WaitForOutcome(context.Background(), "dialog", 2*time.Second)
	reads := stub.count("get_dialogs")
	if reads == 0 {
		t.Fatal("the wait never read the dialog ring, so there is no cadence to measure")
	}
	if outcome.Wakeups != reads {
		t.Fatalf("wakeups = %d but the extension served %d reads", outcome.Wakeups, reads)
	}

	if reads > 14 || reads < 2 {
		t.Fatalf("a 2s dialog wait made %d bridge reads, want between 2 and 14", reads)
	}

	gaps := stub.gaps("get_dialogs")
	if len(gaps) < 4 {
		t.Fatalf("only %d intervals to measure; the cadence cannot be checked", len(gaps))
	}

	if gaps[0] > 150*time.Millisecond {
		t.Fatalf("first interval %s, want it near the %s start", gaps[0], waitFallbackPollStart)
	}
	if last := gaps[len(gaps)-1]; last < 300*time.Millisecond {
		t.Fatalf("last interval %s, want it to have backed off toward the %s ceiling", last, waitFallbackPollMax)
	}
}
