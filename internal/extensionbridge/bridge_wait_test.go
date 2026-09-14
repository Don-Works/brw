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

// rpcStub is a fake extension plus the record of what the bridge actually asked
// it. The record is the point: a wait that answers from the in-page fallback also
// times out and also reports a small wakeup count, so an assertion on the error
// text alone cannot tell the download/dialog paths from their absence. The RPCs
// served can.
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

// gaps returns the intervals between successive calls of one type, which is the
// cadence the bridge actually ran rather than the cadence it intended.
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

// serveRPCStub connects a fake extension that answers each RPC from reply, which
// is called with the message type and how many times that type has been asked.
// Returning ok=false sends the error form the bridge's capability checks read.
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

// The extension transport has no debugger attached, so it cannot subscribe to
// Browser.downloadProgress or Page.javascriptDialogOpening. The same waits must
// still resolve, and must say plainly that they cost a re-ask per check.
func TestBridgeDownloadWaitResolvesByPolling(t *testing.T) {
	b := New("", 5*time.Second, "")
	// The download is still running for the first two reads, so the wait cannot
	// resolve on its baseline read and has to come back for more.
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

// A download that was already finished when the wait started is the baseline, not
// the answer — unless it finished inside the recency window, in which case it is
// the one the caller's click just caused. That is the direct-CDP rule
// (Manager.downloadSettledBefore), and docs/waiting.md says both transports apply
// it, so the extension has to apply it to its own change timestamps.
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
			// Either way the answer has to come from the download registry. The
			// in-page fallback times out too, so without this the test cannot tell
			// the download path from its absence.
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

// A transport that cannot answer at all returns the NAMED capability error rather
// than silently timing out, so the caller learns why instead of reading a timeout
// as "the download failed" or "no dialog opened".
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

// A dialog from an earlier step is outside the recency window and is not the one
// the caller's click just raised.
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
	// The in-page fallback times out too, so the reads are what prove the dialog
	// ring was consulted and its record rejected on age rather than never seen.
	if got := stub.count("get_dialogs"); got < 2 {
		t.Fatalf("the extension served %d get_dialogs calls, want the wait to keep re-reading the dialog ring", got)
	}
}

// The polling cadence must back off rather than hammer the message port for the
// whole timeout: each check is one RPC over the bridge.
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
	// At the 60 ms start with no backoff a 2 s wait would be ~33 reads; backing
	// off to a 400 ms ceiling keeps it near ten.
	if reads > 14 || reads < 2 {
		t.Fatalf("a 2s dialog wait made %d bridge reads, want between 2 and 14", reads)
	}

	gaps := stub.gaps("get_dialogs")
	if len(gaps) < 4 {
		t.Fatalf("only %d intervals to measure; the cadence cannot be checked", len(gaps))
	}
	// The cadence has to START tight and END at the ceiling. A fixed 400 ms
	// cadence fails the first check and a fixed 60 ms one fails the second, so
	// between them they pin the backoff rather than just its read count.
	if gaps[0] > 150*time.Millisecond {
		t.Fatalf("first interval %s, want it near the %s start", gaps[0], waitFallbackPollStart)
	}
	if last := gaps[len(gaps)-1]; last < 300*time.Millisecond {
		t.Fatalf("last interval %s, want it to have backed off toward the %s ceiling", last, waitFallbackPollMax)
	}
}
