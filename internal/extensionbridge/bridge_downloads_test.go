package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serveDownloadsStub connects a minimal fake extension that answers a single RPC
// type (get_downloads) with the supplied reply, mirroring the connect pattern in
// bridge_activetab_test.go's connectFakeExtension. reply is the JSON object the
// extension would send back under {id, ok, result|error}.
func serveDownloadsStub(t *testing.T, b *Bridge, ok bool, result map[string]any, errMsg string) func() {
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
			reply := map[string]any{"id": msg.ID, "ok": ok}
			if ok {
				reply["result"] = result
			} else {
				reply["error"] = errMsg
			}
			out, _ := json.Marshal(reply)
			_ = conn.Write(serveCtx, websocket.MessageText, out)
		}
	}()
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

func TestBridgeDownloadsCapturesEntries(t *testing.T) {
	b := New("", 5*time.Second, "")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"supported": true,
		"downloads": []map[string]any{
			{
				"guid":               "42",
				"url":                "https://example.test/report.pdf",
				"suggested_filename": "report.pdf",
				"state":              "completed",
				"received_bytes":     1234,
				"total_bytes":        1234,
				"path":               "/Users/me/Downloads/report.pdf",
			},
		},
	}, "")
	defer cleanup()

	res, err := b.Downloads(context.Background())
	if err != nil {
		t.Fatalf("Downloads: %v", err)
	}
	if !res.Supported {
		t.Fatalf("expected Supported=true, got false (note=%q)", res.Note)
	}
	if res.Count != 1 || len(res.Downloads) != 1 {
		t.Fatalf("expected 1 download, got count=%d len=%d", res.Count, len(res.Downloads))
	}
	d := res.Downloads[0]
	if d.GUID != "42" || d.SuggestedFilename != "report.pdf" || d.State != "completed" {
		t.Fatalf("download fields not parsed: %+v", d)
	}
	if d.Path != "/Users/me/Downloads/report.pdf" || d.TotalBytes != 1234 {
		t.Fatalf("download path/bytes not parsed: %+v", d)
	}
}

func TestBridgeDownloadsGracefulOnOldExtension(t *testing.T) {
	b := New("", 5*time.Second, "")
	// An extension predating issue #6 rejects the message; Downloads must degrade
	// to Supported=false with a note rather than surfacing a hard error.
	cleanup := serveDownloadsStub(t, b, false, nil, "unknown message type get_downloads")
	defer cleanup()

	res, err := b.Downloads(context.Background())
	if err != nil {
		t.Fatalf("expected graceful fallback, got error: %v", err)
	}
	if res.Supported {
		t.Fatalf("expected Supported=false for old extension")
	}
	if res.Note == "" {
		t.Fatalf("expected an explanatory note on unsupported result")
	}
	if res.Downloads == nil {
		t.Fatalf("expected non-nil empty downloads slice")
	}
}
