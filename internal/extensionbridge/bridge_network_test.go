package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

func TestFilterBridgeCapturedRequestsPreservesLifecycleIDAndRedactsCredentials(t *testing.T) {
	requests := []snapshot.CapturedRequest{{
		CaptureID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1",
		URL:       "https://api.example.test/events",
		RequestHeaders: map[string]string{
			"Authorization": "Bearer bridge-secret",
			"Accept":        "application/json",
		},
	}}
	got := filterBridgeCapturedRequests(requests, "/events")
	if len(got) != 1 || got[0].CaptureID != requests[0].CaptureID {
		t.Fatalf("filtered lifecycle identity = %+v", got)
	}
	if got[0].RequestHeaders["Authorization"] != "[redacted]" || got[0].RequestHeaders["Accept"] != "application/json" {
		t.Fatalf("filtered request headers = %+v", got[0].RequestHeaders)
	}
}

func TestBridgeNetworkCaptureUsesSharedLifecycleScripts(t *testing.T) {
	b := New("", 5*time.Second, "")
	expressions, cleanup := serveNetworkCaptureStub(t, b)
	defer cleanup()

	requests, err := b.NetworkCapture(browser.WithTabID(context.Background(), "42"), "/slow")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].CaptureID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1" || requests[0].Completed || requests[0].Status != 0 {
		t.Fatalf("bridge lifecycle result = %+v", requests)
	}
	if requests[0].RequestHeaders["Authorization"] != "[redacted]" {
		t.Fatalf("bridge exposed captured credential: %+v", requests[0].RequestHeaders)
	}
	if install, drain := <-expressions, <-expressions; install != snapshot.NetworkCaptureInstallScript || drain != snapshot.NetworkCaptureDrainScript {
		t.Fatalf("bridge did not execute shared lifecycle scripts: install_match=%v drain_match=%v", install == snapshot.NetworkCaptureInstallScript, drain == snapshot.NetworkCaptureDrainScript)
	}
}

func serveNetworkCaptureStub(t *testing.T, b *Bridge) (<-chan string, func()) {
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
	waitUntil(t, b.liveConn)

	expressions := make(chan string, 2)
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		call := 0
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg request
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			cdpParams, _ := msg.Params["params"].(map[string]any)
			expression, _ := cdpParams["expression"].(string)
			expressions <- expression
			var value any = map[string]any{"installed": true, "version": 2}
			if call == 1 {
				value = []map[string]any{{
					"capture_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1",
					"completed":  false,
					"method":     "GET",
					"url":        "https://example.test/slow",
					"request_headers": map[string]string{
						"Authorization": "Bearer bridge-secret",
					},
					"status": 0,
				}}
			}
			call++
			reply, _ := json.Marshal(map[string]any{
				"id": msg.ID,
				"ok": true,
				"result": map[string]any{
					"result": map[string]any{"value": value},
				},
			})
			_ = conn.Write(serveCtx, websocket.MessageText, reply)
		}
	}()
	return expressions, func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}
