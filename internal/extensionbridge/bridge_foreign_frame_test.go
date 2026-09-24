package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

const (
	testForeignExtensionID = "nngceckbapebfimnlniiiahkandclblb"
	testForeignFrameURL    = "chrome-extension://" + testForeignExtensionID + "/overlay/menu-list.html"
)

// foreignFrameExtension is a fake service worker for a page that embeds another
// extension's frame. It answers the way extension/service_worker.js does once
// Chrome has refused the debugger: page reads come back from chrome.scripting
// with a brwTransport note, and a method with no scripting equivalent fails
// with the named foreign_extension_frame error.
type foreignFrameExtension struct {
	mu            sync.Mutex
	viaScripting  bool
	refuseAll     bool
	skippedFrames int
	cdpCalls      int
}

func (f *foreignFrameExtension) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cdpCalls
}

func (f *foreignFrameExtension) transport() map[string]any {
	return map[string]any{
		"path":                     "scripting",
		"frame_id":                 0,
		"skipped_extension_frames": 1,
		"blocked_by": []map[string]any{{
			"frame_id": 9, "url": testForeignFrameURL, "extension_id": testForeignExtensionID,
		}},
	}
}

func (f *foreignFrameExtension) refusal(tabID int) string {
	return "foreign_extension_frame: Chrome refuses brw's debugger for tab " + itoa(tabID) +
		" while the page embeds a frame from another extension (extension " + testForeignExtensionID + " at " + testForeignFrameURL +
		"). Chrome said: Cannot access a chrome-extension:// URL of different extension"
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func (f *foreignFrameExtension) serve(ctx context.Context, conn *websocket.Conn) {
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
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		f.mu.Lock()
		viaScripting, refuseAll, skipped := f.viaScripting, f.refuseAll, f.skippedFrames
		f.mu.Unlock()
		reply := map[string]any{"id": msg.ID, "ok": true, "result": map[string]any{}}
		switch msg.Type {
		case "get_active_tab_id":
			reply["result"] = map[string]any{"tabId": 51}
		case "list_tabs":
			reply["result"] = []map[string]any{{
				"id": 51, "url": "https://beta.test/contact", "title": "Contact",
				"active": true, "windowId": 1, "windowFocused": true, "windowType": "normal",
			}}
		case "read_cross_origin_frames":
			result := map[string]any{"frames": []any{}}
			if skipped > 0 {
				result["skippedExtensionFrames"] = skipped
			}
			reply["result"] = result
		case "cdp":
			f.mu.Lock()
			f.cdpCalls++
			f.mu.Unlock()
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			expression, _ := params["expression"].(string)
			if refuseAll || method != "Runtime.evaluate" {
				reply["ok"] = false
				reply["error"] = f.refusal(51)
				break
			}
			var value any = map[string]any{"ok": true}
			if isSnapshotWalkExpression(expression) {
				value = map[string]any{
					"url":      "https://beta.test/contact",
					"title":    "Contact",
					"elements": []map[string]any{{"ref": "e1", "role": "button", "name": "Send", "tag": "button"}},
					"metadata": map[string]any{"version": 1},
				}
			} else if strings.Contains(expression, "6 * 7") {
				value = 42
			}
			result := map[string]any{"result": map[string]any{"type": "object", "value": value}}
			if viaScripting {
				result["brwTransport"] = f.transport()
			}
			reply["result"] = result
		}
		out, _ := json.Marshal(reply)
		_ = conn.Write(ctx, websocket.MessageText, out)
	}
}

func connectForeignFrameExtension(t *testing.T, b *Bridge, f *foreignFrameExtension) func() {
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
	conn.SetReadLimit(8 << 20)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go f.serve(serveCtx, conn)
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

func TestSnapshotNotesAPageReadAroundAForeignExtensionFrame(t *testing.T) {
	cases := []struct {
		name          string
		viaScripting  bool
		includeFrames bool
		skippedFrames int
		wantSkipped   int
		wantTransport string
		wantNote      string
	}{
		{name: "debugger path, nothing skipped"},
		{
			name:          "read through chrome.scripting",
			viaScripting:  true,
			wantSkipped:   1,
			wantTransport: "scripting",
			wantNote:      "read through chrome.scripting in the top frame, and 1 frame(s) belonging to other extensions were skipped",
		},
		{
			name:          "frame enumeration skipped extension frames",
			includeFrames: true,
			skippedFrames: 2,
			wantSkipped:   2,
			wantNote:      "2 frame(s) belonging to other browser extensions were skipped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			ext := &foreignFrameExtension{viaScripting: tc.viaScripting, skippedFrames: tc.skippedFrames}
			defer connectForeignFrameExtension(t, b, ext)()
			ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "51"), 20*time.Second)
			defer cancel()

			snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeFrames: tc.includeFrames})
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			if len(snap.Elements) == 0 || snap.Elements[0].Name != "Send" {
				t.Fatalf("the page's own controls were not read: %+v", snap.Elements)
			}
			if tc.wantSkipped == 0 {
				for _, key := range []string{"skipped_extension_frames", "page_transport", "frames_note"} {
					if _, ok := snap.Metadata[key]; ok {
						t.Fatalf("metadata[%q] set on an ordinary debugger read: %v", key, snap.Metadata)
					}
				}
				return
			}
			if got := snap.Metadata["skipped_extension_frames"]; got != tc.wantSkipped {
				t.Fatalf("skipped_extension_frames = %v, want %d", got, tc.wantSkipped)
			}
			if got, _ := snap.Metadata["page_transport"].(string); got != tc.wantTransport {
				t.Fatalf("page_transport = %q, want %q", got, tc.wantTransport)
			}
			note, _ := snap.Metadata["frames_note"].(string)
			if !strings.Contains(note, tc.wantNote) {
				t.Fatalf("frames_note = %q, want it to contain %q", note, tc.wantNote)
			}
			if tc.viaScripting {
				ids, _ := snap.Metadata["skipped_extension_ids"].([]string)
				if len(ids) != 1 || ids[0] != testForeignExtensionID {
					t.Fatalf("skipped_extension_ids = %v, want [%s]", snap.Metadata["skipped_extension_ids"], testForeignExtensionID)
				}
				found, err := b.Find(ctx, snapshot.FindOptions{Query: "Send"})
				if err != nil {
					t.Fatalf("find: %v", err)
				}
				if found.Metadata["page_transport"] != "scripting" {
					t.Fatalf("find did not carry the transport note: %v", found.Metadata)
				}
			}
		})
	}
}

func TestEvaluateAnswersThroughScriptingAroundAForeignExtensionFrame(t *testing.T) {
	b := New("", 5*time.Second, "")
	ext := &foreignFrameExtension{viaScripting: true}
	defer connectForeignFrameExtension(t, b, ext)()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "51"), 20*time.Second)
	defer cancel()

	value, err := b.Evaluate(ctx, "6 * 7")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if n, _ := value.(float64); n != 42 {
		t.Fatalf("evaluate = %v, want 42", value)
	}
}

func TestForeignExtensionFrameRefusalIsANamedError(t *testing.T) {
	b := New("", 5*time.Second, "")
	ext := &foreignFrameExtension{refuseAll: true}
	defer connectForeignFrameExtension(t, b, ext)()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "51"), 20*time.Second)
	defer cancel()

	_, err := b.Evaluate(ctx, "document.title")
	if err == nil {
		t.Fatal("evaluate succeeded on a tab the extension refused")
	}
	if !errors.Is(err, browser.ErrForeignExtensionFrame) {
		t.Fatalf("error is not ErrForeignExtensionFrame: %v", err)
	}
	for _, want := range []string{testForeignExtensionID, testForeignFrameURL, "tab 51", "foreign_extension_frame: Chrome refuses"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if calls := ext.calls(); calls != 1 {
		t.Fatalf("the refusal was sent %d CDP calls; it is not transient and must not be retried", calls)
	}
}
