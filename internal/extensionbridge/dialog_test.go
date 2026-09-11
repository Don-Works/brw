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
	"github.com/coder/websocket"
)

// scriptedExtension answers exactly one bridge command with a caller-supplied
// result, and hands back the command frame that was sent.
//
// Distinct from fakeExtension in bridge_activetab_test.go on purpose: that one
// models a live browser's tabs and answers the tab RPCs from that model, which
// is what tab-resolution tests need. These tests are about the WIRE — the exact
// keys and types crossing the socket in each direction — so the reply has to be
// literal rather than derived from a model.
type scriptedExtension struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

func newScriptedExtension(t *testing.T, b *Bridge) *scriptedExtension {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test done") })

	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	return &scriptedExtension{t: t, conn: conn, ctx: context.Background()}
}

// serveOnce reads the next bridge command and answers it with result.
func (f *scriptedExtension) serveOnce(result map[string]any) (cmdType string, params map[string]any) {
	f.t.Helper()
	readCtx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	_, data, err := f.conn.Read(readCtx)
	if err != nil {
		f.t.Fatalf("read bridge command: %v", err)
	}
	var cmd struct {
		ID     string         `json:"id"`
		Type   string         `json:"type"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		f.t.Fatalf("unmarshal bridge command: %v", err)
	}
	reply, _ := json.Marshal(map[string]any{"id": cmd.ID, "ok": true, "result": result})
	if err := f.conn.Write(readCtx, websocket.MessageText, reply); err != nil {
		f.t.Fatalf("write reply: %v", err)
	}
	return cmd.Type, cmd.Params
}

// The extension emits the dialog record as JSON and the daemon unmarshals it
// into browser.DialogRecord. Those two spellings are a wire contract that no
// compiler checks: a camelCase key silently blanks the field, and a numeric
// timestamp fails the whole parse. This asserts the daemon reads the exact
// shape extension/service_worker.js emits (pinned on that side by
// extension/tab_resolution_test.mjs).
func TestBridgeDialogStatusParsesTheExtensionWireShape(t *testing.T) {
	b := New("", 5*time.Second, "")
	ext := newScriptedExtension(t, b)

	type out struct {
		result browser.DialogResult
		err    error
	}
	done := make(chan out, 1)
	go func() {
		result, err := b.Dialog(context.Background(), browser.DialogOptions{Action: "status", TabID: "77"})
		done <- out{result, err}
	}()

	cmdType, params := ext.serveOnce(map[string]any{
		"dialogs": []map[string]any{{
			"type":           "confirm",
			"message":        "Really delete?",
			"default_prompt": "",
			"url":            "https://app.test/",
			"accepted":       false,
			"prompt_text":    "",
			"decided_by":     "user_safe_default",
			"at":             "2026-09-11T19:00:00.000Z",
		}},
		"count": 1,
		"armed": map[string]any{"accept": true, "remaining": 2, "prompt_text": "typed"},
	})
	if cmdType != "get_dialogs" {
		t.Fatalf("bridge command type = %q, want get_dialogs", cmdType)
	}
	if params["tabId"] != float64(77) {
		t.Fatalf("bridge did not forward the tab id: %#v", params)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Dialog returned error: %v", got.err)
		}
		if got.result.Count != 1 || len(got.result.Dialogs) != 1 {
			t.Fatalf("result = %#v, want one dialog", got.result)
		}
		record := got.result.Dialogs[0]
		// decided_by is the field the whole feature is for: it says why the
		// dialog was answered the way it was.
		if record.DecidedBy != "user_safe_default" {
			t.Errorf("decided_by = %q, want user_safe_default", record.DecidedBy)
		}
		if record.Type != "confirm" || record.Message != "Really delete?" {
			t.Errorf("record = %#v", record)
		}
		if record.Accepted {
			t.Error("accepted should be false for a safe-default confirm")
		}
		if record.At == "" {
			t.Error("at must survive the wire as a string")
		}
		if got.result.Armed == nil || got.result.Armed.Remaining != 2 || got.result.Armed.PromptText != "typed" {
			t.Errorf("armed = %#v, want the pending arm with its prompt text", got.result.Armed)
		}
		if !got.result.Supported {
			t.Error("a bridge that answered should report supported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dialog did not return")
	}
}

func TestBridgeDialogExpectSendsTheArm(t *testing.T) {
	tests := []struct {
		name       string
		opts       browser.DialogOptions
		wantAccept bool
		wantCount  any
		wantPrompt any
	}{
		{
			name:       "accept",
			opts:       browser.DialogOptions{Action: "expect", Response: "accept", TabID: "5"},
			wantAccept: true,
		},
		{
			name:       "dismiss",
			opts:       browser.DialogOptions{Action: "expect", Response: "dismiss", TabID: "5"},
			wantAccept: false,
		},
		{
			name:       "accept with prompt text and a count",
			opts:       browser.DialogOptions{Action: "expect", Response: "accept", PromptText: "hello", Count: 3, TabID: "5"},
			wantAccept: true,
			wantCount:  float64(3),
			wantPrompt: "hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			ext := newScriptedExtension(t, b)

			done := make(chan error, 1)
			go func() {
				_, err := b.Dialog(context.Background(), tt.opts)
				done <- err
			}()

			cmdType, params := ext.serveOnce(map[string]any{"armed": true, "accept": tt.wantAccept, "remaining": 1})
			if cmdType != "arm_dialog" {
				t.Fatalf("command type = %q, want arm_dialog", cmdType)
			}
			if params["accept"] != tt.wantAccept {
				t.Errorf("accept = %#v, want %v", params["accept"], tt.wantAccept)
			}
			if tt.wantCount != nil && params["count"] != tt.wantCount {
				t.Errorf("count = %#v, want %v", params["count"], tt.wantCount)
			}
			if tt.wantPrompt != nil && params["promptText"] != tt.wantPrompt {
				t.Errorf("promptText = %#v, want %v", params["promptText"], tt.wantPrompt)
			}
			if err := <-done; err != nil {
				t.Fatalf("Dialog returned error: %v", err)
			}
		})
	}
}

func TestBridgeDialogRejectsBadInputBeforeTouchingTheWire(t *testing.T) {
	b := New("", 5*time.Second, "")
	for _, tt := range []struct {
		name    string
		opts    browser.DialogOptions
		wantErr string
	}{
		{"unknown action", browser.DialogOptions{Action: "teleport"}, "unknown dialog action"},
		{"expect without response", browser.DialogOptions{Action: "expect"}, "requires response"},
		{"expect with bad response", browser.DialogOptions{Action: "expect", Response: "maybe"}, "unknown dialog response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// No extension is connected: a well-formed rejection must not depend
			// on one, and must not hang waiting for a reply.
			_, err := b.Dialog(context.Background(), tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// A blocked request crosses the same wire as the dialog record and follows the
// same convention, so it is pinned the same way.
func TestBridgeBlockedRequestsParsesTheExtensionWireShape(t *testing.T) {
	b := New("", 5*time.Second, "")
	ext := newScriptedExtension(t, b)

	type out struct {
		blocked []browser.BlockedRequest
		err     error
	}
	done := make(chan out, 1)
	go func() {
		blocked, err := b.BlockedRequests(context.Background(), "12")
		done <- out{blocked, err}
	}()

	cmdType, _ := ext.serveOnce(map[string]any{
		"blocked": []map[string]any{{
			"url":           "https://tracker.example/px.gif",
			"resource_type": "Image",
			"reason":        "not permitted by the brw navigation policy",
			"at":            "2026-09-11T19:00:00.000Z",
		}},
		"count": 1,
	})
	if cmdType != "get_blocked_requests" {
		t.Fatalf("command type = %q, want get_blocked_requests", cmdType)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("BlockedRequests returned error: %v", got.err)
		}
		if len(got.blocked) != 1 {
			t.Fatalf("blocked = %#v, want one entry", got.blocked)
		}
		if got.blocked[0].ResourceType != "Image" || got.blocked[0].URL == "" || got.blocked[0].At == "" {
			t.Errorf("blocked entry lost fields on the wire: %#v", got.blocked[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BlockedRequests did not return")
	}
}
