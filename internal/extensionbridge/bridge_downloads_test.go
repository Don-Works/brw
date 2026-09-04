package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
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

func serveDownloadsSequenceStub(t *testing.T, b *Bridge, results []map[string]any) func() {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("downloads result sequence must not be empty")
	}
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
		call := 0
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			index := min(call, len(results)-1)
			call++
			out, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": results[index]})
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

func TestBridgeDownloadsRecipeUsesPerTabChangeCursor(t *testing.T) {
	entry := func(guid, tabID, state string) map[string]any {
		return map[string]any{
			"guid": guid, "url": "https://example.test/invoice.pdf", "suggested_filename": "invoice.pdf",
			"tab_id": tabID, "state": state, "path": "/tmp/" + guid,
		}
	}
	old := entry("old", "7", "completed")
	fresh := entry("fresh", "7", "inProgress")
	otherTab := entry("other-tab", "8", "completed")
	unknown := entry("unknown", "", "completed")
	completed := entry("fresh", "7", "completed")
	results := []map[string]any{
		{"supported": true, "downloads": []map[string]any{old}},
		{"supported": true, "downloads": []map[string]any{old}},
		{"supported": true, "downloads": []map[string]any{old, fresh, otherTab, unknown}},
		{"supported": true, "downloads": []map[string]any{old, completed, otherTab, unknown}},
		{"supported": true, "downloads": []map[string]any{old, completed, otherTab, unknown}},
		{"supported": true, "downloads": []map[string]any{old, completed, otherTab, unknown}},
	}
	b := New("", 5*time.Second, "")
	cleanup := serveDownloadsSequenceStub(t, b, results)
	defer cleanup()

	ctx := browser.WithAllowedOrigins(browser.WithTabID(context.Background(), "7"), []string{"https://example.test"})
	baseline, err := b.Downloads(ctx)
	if err != nil || baseline.Count != 1 || baseline.Downloads[0].GUID != "old" {
		t.Fatalf("baseline=%+v err=%v", baseline, err)
	}
	unchanged, err := b.Downloads(ctx)
	if err != nil || unchanged.Count != 0 {
		t.Fatalf("unchanged=%+v err=%v", unchanged, err)
	}
	delta, err := b.Downloads(ctx)
	if err != nil || delta.Count != 1 || delta.Downloads[0].GUID != "fresh" || delta.Downloads[0].State != "inProgress" {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
	completion, err := b.Downloads(ctx)
	if err != nil || completion.Count != 1 || completion.Downloads[0].GUID != "fresh" || completion.Downloads[0].State != "completed" {
		t.Fatalf("completion=%+v err=%v", completion, err)
	}

	manual, err := b.Downloads(context.Background())
	if err != nil || manual.Count != 4 {
		t.Fatalf("manual snapshot=%+v err=%v", manual, err)
	}
	repeated, err := b.Downloads(context.Background())
	if err != nil || repeated.Count != 4 || repeated.Downloads[1].GUID != "fresh" {
		t.Fatalf("repeated snapshot=%+v err=%v", repeated, err)
	}
}

func TestBridgePublicDownloadsThenArtifactCaptureRetainsUserOriginal(t *testing.T) {
	downloadPath := filepath.Join(t.TempDir(), "invoice.pdf")
	payload := []byte("%PDF-1.7\nextension user download\n")
	if err := os.WriteFile(downloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	b := New("", 5*time.Second, "")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"supported": true,
		"downloads": []map[string]any{{
			"guid": "91", "url": "https://example.test/invoice.pdf", "suggested_filename": "invoice.pdf",
			"tab_id": "7", "state": "completed", "path": downloadPath,
		}},
	}, "")
	defer cleanup()

	listed, err := b.Downloads(context.Background())
	if err != nil || listed.Count != 1 || listed.Downloads[0].GUID != "91" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	store, err := artifact.NewStore(artifact.Config{
		Root: t.TempDir(), MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(store, b)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureArtifact(context.Background(), artifact.CaptureOptions{Kind: "download", DownloadGUID: "91"})
	if err != nil || meta.SizeBytes != int64(len(payload)) || meta.MIMEType != "application/pdf" {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	if got, err := os.ReadFile(downloadPath); err != nil || string(got) != string(payload) {
		t.Fatalf("extension user original changed: data=%q err=%v", got, err)
	}
}
