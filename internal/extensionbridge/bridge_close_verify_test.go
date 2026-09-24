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

	"github.com/coder/websocket"
)

// closeFailFakeExtension answers close_tab with a fixed error and reports the
// closed tab in list_tabs for the first stillListedFor list calls.
type closeFailFakeExtension struct {
	mu             sync.Mutex
	closeError     string
	stillListedFor int
	listCalls      int
}

func (f *closeFailFakeExtension) serve(ctx context.Context, conn *websocket.Conn, tabID int) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		reply := map[string]any{"id": msg.ID, "ok": true}
		f.mu.Lock()
		switch msg.Type {
		case "close_tab":
			reply["ok"] = false
			reply["error"] = f.closeError
		case "list_tabs":
			f.listCalls++
			tabs := []map[string]any{{"id": 1, "url": "https://other.test", "windowId": 1, "windowType": "normal"}}
			if f.listCalls <= f.stillListedFor {
				tabs = append(tabs, map[string]any{"id": tabID, "url": "https://mail.test", "windowId": 1, "windowType": "normal"})
			}
			reply["result"] = tabs
		default:
			reply["result"] = map[string]any{}
		}
		f.mu.Unlock()
		out, _ := json.Marshal(reply)
		_ = conn.Write(ctx, websocket.MessageText, out)
	}
}

func TestCloseTabReportsTheTabsActualState(t *testing.T) {
	const tabID = 235941502
	cases := []struct {
		name           string
		closeError     string
		stillListedFor int
		wantErr        bool
	}{
		{
			name:       "tab gone when the extension gave up waiting",
			closeError: "tab 235941502 did not close within 2000ms",
		},
		{
			name:           "tab leaves shortly after the extension gave up",
			closeError:     "tab 235941502 did not close within 2000ms",
			stillListedFor: 2,
		},
		{
			name:       "tab already closed by someone else",
			closeError: "No tab with id: 235941502.",
		},
		{
			name:           "tab still open after the verify window",
			closeError:     "tab 235941502 did not close within 2000ms",
			stillListedFor: 1 << 20,
			wantErr:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 10*time.Second, "")
			srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
			defer srv.Close()
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
			dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dialCancel()
			conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
				HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
			})
			if err != nil {
				t.Fatalf("dial bridge: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "test done")
			waitUntil(t, func() bool {
				b.mu.RLock()
				defer b.mu.RUnlock()
				return b.conn != nil
			})
			fe := &closeFailFakeExtension{closeError: tc.closeError, stillListedFor: tc.stillListedFor}
			serveCtx, serveCancel := context.WithCancel(context.Background())
			defer serveCancel()
			go fe.serve(serveCtx, conn, tabID)

			err = b.CloseTab(context.Background(), "235941502")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "did not close") {
					t.Fatalf("CloseTab error = %v, want the extension's close failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CloseTab error = %v, want nil because the tab is gone", err)
			}
		})
	}
}

func TestCloseTabDoesNotVerifyAfterTheCallerGaveUp(t *testing.T) {
	b := New("", 5*time.Second, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if b.tabGoneAfterFailedClose(ctx, 42) {
		t.Fatal("a cancelled caller must not have a failed close reported as success")
	}
}
