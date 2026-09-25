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

	"github.com/Don-Works/brw/internal/snapshot"
)

// webmcpExtension answers set_webmcp the way the service worker does, or with
// "unknown message type" the way an extension older than 0.7.7 does, and
// records every set_webmcp it was sent.
type webmcpExtension struct {
	mu      sync.Mutex
	legacy  bool
	failing bool
	arms    []map[string]any
}

func (f *webmcpExtension) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.arms)
}

func (f *webmcpExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
		reply := map[string]any{"id": msg.ID, "ok": true, "result": map[string]any{}}
		if msg.Type == "set_webmcp" {
			f.mu.Lock()
			f.arms = append(f.arms, msg.Params)
			legacy, failing := f.legacy, f.failing
			f.mu.Unlock()
			switch {
			case legacy:
				reply = map[string]any{"id": msg.ID, "ok": false, "error": unknownMessageType + ": set_webmcp"}
			case failing:
				reply = map[string]any{"id": msg.ID, "ok": false, "error": "cannot control tab"}
			default:
				reply["result"] = map[string]any{"enabled": true, "armed": true}
			}
		}
		out, _ := json.Marshal(reply)
		_ = conn.Write(ctx, websocket.MessageText, out)
	}
}

// connectWebMCPExtension attaches ext to b over a fresh socket and returns a
// function that drops that socket.
func connectWebMCPExtension(t *testing.T, b *Bridge, ext *webmcpExtension) func() {
	t.Helper()
	b.mu.RLock()
	previous := b.conn
	b.mu.RUnlock()
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
		return b.conn != nil && b.conn != previous
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go ext.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

func TestEnsureWebMCPArmsOncePerTabPerConnection(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		legacy     bool
		failing    bool
		calls      []string
		wantArms   int
		wantSource bool
	}{
		{name: "disabled sends nothing", enabled: false, calls: []string{"7", "7"}, wantArms: 0},
		{name: "one tab is armed once", enabled: true, calls: []string{"7", "7", "7"}, wantArms: 1, wantSource: true},
		{name: "each tab is armed", enabled: true, calls: []string{"7", "8", "7"}, wantArms: 2, wantSource: true},
		{name: "an empty tab id is skipped", enabled: true, calls: []string{"", " "}, wantArms: 0},
		{name: "an old extension is asked once, not per call", enabled: true, legacy: true, calls: []string{"7", "7"}, wantArms: 1, wantSource: true},
		{name: "a failed arm is retried", enabled: true, failing: true, calls: []string{"7", "7"}, wantArms: 2, wantSource: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			b.SetWebMCP(tc.enabled)
			ext := &webmcpExtension{legacy: tc.legacy, failing: tc.failing}
			drop := connectWebMCPExtension(t, b, ext)
			defer drop()
			for _, tab := range tc.calls {
				b.ensureWebMCP(context.Background(), tab)
			}
			if got := ext.count(); got != tc.wantArms {
				t.Fatalf("set_webmcp sent %d times, want %d", got, tc.wantArms)
			}
			if tc.wantSource {
				ext.mu.Lock()
				first := ext.arms[0]
				ext.mu.Unlock()
				if first["source"] != snapshot.WebMCPInstallScript || first["catchUp"] != true || first["enabled"] != true {
					t.Fatalf("set_webmcp params = %v, want the install script with catch-up", first)
				}
			}
		})
	}
}

// A new socket can be a restarted service worker that lost every arm, so the
// daemon must arm again rather than trust its record of the old worker.
func TestEnsureWebMCPRearmsAfterReconnect(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetWebMCP(true)
	first := &webmcpExtension{}
	dropFirst := connectWebMCPExtension(t, b, first)
	b.ensureWebMCP(context.Background(), "7")
	b.ensureWebMCP(context.Background(), "7")
	if got := first.count(); got != 1 {
		t.Fatalf("first worker armed %d times, want 1", got)
	}
	dropFirst()

	second := &webmcpExtension{}
	dropSecond := connectWebMCPExtension(t, b, second)
	defer dropSecond()
	b.ensureWebMCP(context.Background(), "7")
	if got := second.count(); got != 1 {
		t.Fatalf("restarted worker armed %d times, want 1", got)
	}
}

func TestOpenTabParamsCarryTheShimOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		b := New("", time.Second, "")
		b.SetWebMCP(enabled)
		params := b.openTabParams(map[string]any{"url": "https://example.test/"})
		source, has := params["webmcp"]
		if has != enabled {
			t.Fatalf("enabled=%v: webmcp param present=%v", enabled, has)
		}
		if enabled && source != snapshot.WebMCPInstallScript {
			t.Fatalf("webmcp param is not the install script")
		}
	}
}

func TestNoteOpenedWebMCPTrustsAnArmedOpen(t *testing.T) {
	cases := []struct {
		name     string
		armed    bool
		wantArms int
	}{
		{name: "armed by open_tab needs no second message", armed: true, wantArms: 0},
		{name: "not armed by open_tab gets the catch-up arm", armed: false, wantArms: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			b.SetWebMCP(true)
			ext := &webmcpExtension{}
			drop := connectWebMCPExtension(t, b, ext)
			defer drop()
			b.noteOpenedWebMCP(context.Background(), "9", tc.armed)
			b.ensureWebMCP(context.Background(), "9")
			if got := ext.count(); got != tc.wantArms {
				t.Fatalf("set_webmcp sent %d times, want %d", got, tc.wantArms)
			}
		})
	}
}
