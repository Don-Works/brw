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

// emulationFake records the CDP payloads the bridge sends so a test can assert
// on the wire form rather than on the config struct that produced it.
type emulationFake struct {
	mu    sync.Mutex
	calls map[string]map[string]any
}

func (f *emulationFake) payload(method string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	params, ok := f.calls[method]
	return params, ok
}

func (f *emulationFake) serve(ctx context.Context, conn *websocket.Conn) {
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
		f.mu.Lock()
		switch msg.Type {
		case "get_active_tab_id":
			result = map[string]any{"tabId": 7}
		case "cdp":
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			f.calls[method] = params
			result = map[string]any{"result": map[string]any{"value": map[string]any{}}}
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func connectEmulationFake(t *testing.T) (*Bridge, *emulationFake, func()) {
	t.Helper()
	b := New("", 4*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		srv.Close()
		t.Fatalf("dial emulation fake: %v", err)
	}
	waitUntil(t, b.liveConn)
	fake := &emulationFake{calls: map[string]map[string]any{}}
	serveCtx, cancel := context.WithCancel(context.Background())
	go fake.serve(serveCtx, conn)
	return b, fake, func() {
		cancel()
		_ = conn.CloseNow()
		srv.Close()
	}
}

// Chrome rejects Emulation.setTouchEmulationEnabled with maxTouchPoints outside
// 1-16, so a desktop request that sends the zero value fails with "Touch points
// must be between 1 and 16" instead of turning touch off. Direct CDP omits the
// field; the bridge has to as well or the two transports disagree.
func TestBridgeTouchEmulationOmitsZeroMaxTouchPoints(t *testing.T) {
	tests := []struct {
		name              string
		opts              browser.DeviceEmulationOptions
		wantEnabled       bool
		wantMaxTouchField bool
	}{
		{
			name:              "desktop turns touch off and sends no max",
			opts:              browser.DeviceEmulationOptions{Width: 1440, Height: 900, Mobile: boolPtr(false), Touch: boolPtr(false)},
			wantEnabled:       false,
			wantMaxTouchField: false,
		},
		{
			name:              "mobile keeps its touch points",
			opts:              browser.DeviceEmulationOptions{Width: 400, Height: 900, Mobile: boolPtr(true), Touch: boolPtr(true)},
			wantEnabled:       true,
			wantMaxTouchField: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, fake, done := connectEmulationFake(t)
			defer done()

			if _, err := b.EmulateDevice(context.Background(), tc.opts); err != nil {
				t.Fatalf("EmulateDevice() error = %v", err)
			}
			params, ok := fake.payload("Emulation.setTouchEmulationEnabled")
			if !ok {
				t.Fatal("bridge never sent Emulation.setTouchEmulationEnabled")
			}
			if enabled, _ := params["enabled"].(bool); enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			max, present := params["maxTouchPoints"]
			if present != tc.wantMaxTouchField {
				t.Fatalf("maxTouchPoints present = %v (%v), want present = %v", present, max, tc.wantMaxTouchField)
			}
			if present {
				if points, _ := max.(float64); points < 1 || points > 16 {
					t.Fatalf("maxTouchPoints = %v, want between 1 and 16", max)
				}
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
