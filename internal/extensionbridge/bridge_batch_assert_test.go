package extensionbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/coder/websocket"
)

const (
	assertFakeTabID   = 42
	assertFakePageURL = "https://app.test/report/42"
)

// assertFakeExtension answers the shared getters script by the question its
// expression ends with — ("url","",""), ("state","btn-3","") — so a batch assert
// step is driven through the bridge's real evaluate path rather than a stub of
// it, and the recorded questions prove which getter each assertion kind read.
type assertFakeExtension struct {
	mu        sync.Mutex
	getters   map[string]any
	downloads any
	asked     []string
}

func (f *assertFakeExtension) questionsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func assertFakeSnapshot() map[string]any {
	return map[string]any{
		"url": assertFakePageURL, "title": "Report", "elements": []any{},
		"metadata": map[string]any{"version": 1},
	}
}

func (f *assertFakeExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg request
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		var result any = map[string]any{}
		ok := true
		errText := ""
		f.mu.Lock()
		switch msg.Type {
		case "get_active_tab_id":
			result = map[string]any{"tabId": assertFakeTabID}
		case "list_tabs":
			result = []map[string]any{{
				"id": assertFakeTabID, "url": assertFakePageURL, "title": "Report",
				"active": true, "windowId": 1, "windowFocused": true, "windowType": "normal",
			}}
		case "get_downloads":
			if f.downloads == nil {
				ok = false
				errText = "unknown message type get_downloads"
				break
			}
			result = f.downloads
		case "cached_snapshot":
			result = map[string]any{"cached": true, "snapshot": assertFakeSnapshot()}
		case "snapshot_result":
			result = map[string]any{"stored": true}
		case "cdp":
			params, _ := msg.Params["params"].(map[string]any)
			expression, _ := params["expression"].(string)
			switch {
			case strings.HasPrefix(expression, snapshot.GetScript):
				question := strings.TrimPrefix(expression, snapshot.GetScript)
				f.asked = append(f.asked, question)
				value, known := f.getters[question]
				if !known {
					ok = false
					errText = "fake extension has no answer for " + question
					break
				}
				result = map[string]any{"result": map[string]any{"value": map[string]any{"value": value}}}
			case strings.Contains(expression, "MutationObserver"):
				result = map[string]any{"result": map[string]any{"value": true}}
			default:
				result = map[string]any{"result": map[string]any{"value": assertFakeSnapshot()}}
			}
		default:
			ok = false
			errText = "unknown message type " + msg.Type
		}
		f.mu.Unlock()
		reply, _ := json.Marshal(response{ID: msg.ID, OK: ok, Result: mustAssertFakeJSON(result), Error: errText})
		_ = conn.Write(ctx, websocket.MessageText, reply)
	}
}

func mustAssertFakeJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func connectAssertFake(t *testing.T, fake *assertFakeExtension) (*Bridge, func()) {
	t.Helper()
	b := New("", 5*time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	conn.SetReadLimit(4 << 20)
	waitUntil(t, b.liveConn)
	serveCtx, cancel := context.WithCancel(context.Background())
	go fake.serve(serveCtx, conn)
	return b, func() {
		cancel()
		_ = conn.CloseNow()
		srv.Close()
	}
}

// TestBatchAssertStepRunsOnTheExtensionBridge is the regression test for the
// assert step being advertised on both transports while only direct CDP
// implemented it: on the bridge every one of these returned `unknown action
// "assert"` mid-batch instead of reading the page. The download row covers the
// one kind the bridge genuinely cannot always answer — it must name the missing
// capability rather than pass.
func TestBatchAssertStepRunsOnTheExtensionBridge(t *testing.T) {
	one := 1
	bytes := int64(11)
	cases := []struct {
		name      string
		step      browser.BatchStep
		getters   map[string]any
		downloads any
		wantOK    bool
		wantError string
		wantAsked []string
	}{
		{
			name:      "url assertion passes",
			step:      browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{Assertion: browser.AssertionURL, Expected: assertFakePageURL}},
			getters:   map[string]any{`("url","","")`: assertFakePageURL},
			wantOK:    true,
			wantAsked: []string{`("url","","")`},
		},
		{
			name:      "url assertion failure names expected and actual",
			step:      browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{Assertion: browser.AssertionURL, Expected: "https://app.test/report/7"}},
			getters:   map[string]any{`("url","","")`: assertFakePageURL},
			wantError: `url assertion failed: expected exact "https://app.test/report/7", actual "https://app.test/report/42"`,
			wantAsked: []string{`("url","","")`},
		},
		{
			name:      "http status assertion reads the navigation status",
			step:      browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{Assertion: browser.AssertionHTTPStatus, Status: 200}},
			getters:   map[string]any{`("status","","")`: 200},
			wantOK:    true,
			wantAsked: []string{`("status","","")`},
		},
		{
			name:      "element count assertion counts a selector",
			step:      browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{Assertion: browser.AssertionElementCount, Selector: "li.row", Count: &one}},
			getters:   map[string]any{`("count","li.row","")`: 1},
			wantOK:    true,
			wantAsked: []string{`("count","li.row","")`},
		},
		{
			name: "element state assertion reads the state getter",
			step: browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{Assertion: browser.AssertionElementState, Ref: "btn-3", State: browser.AssertStateEnabled}},
			getters: map[string]any{
				`("state","btn-3","")`: map[string]any{"found": true, "visible": true, "enabled": true},
			},
			wantOK:    true,
			wantAsked: []string{`("state","btn-3","")`},
		},
		{
			name: "attribute assertion resolves the element then the attribute",
			step: browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{
				Assertion: browser.AssertionAttribute, Ref: "link-1", Attribute: "href",
				Expected: "/report", Mode: browser.AssertModeContains,
			}},
			getters: map[string]any{
				`("state","link-1","")`:    map[string]any{"found": true, "visible": true, "enabled": true},
				`("attr","link-1","href")`: "/report/42",
			},
			wantOK:    true,
			wantAsked: []string{`("state","link-1","")`, `("attr","link-1","href")`},
		},
		{
			name:      "assert without an assertion is refused by name",
			step:      browser.BatchStep{Action: "assert"},
			wantError: "assert requires assertion",
		},
		{
			name: "download assertion names the missing capability instead of passing",
			step: browser.BatchStep{Action: "assert", Assertion: &browser.AssertRequest{
				Assertion: browser.AssertionDownload, Filename: "report.csv", Bytes: &bytes,
			}},
			downloads: map[string]any{
				"downloads": []any{}, "supported": false,
				"note": "the connected extension predates chrome.downloads support",
			},
			wantError: "download digest assertions are unavailable on this transport: the connected extension predates chrome.downloads support",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &assertFakeExtension{getters: tc.getters, downloads: tc.downloads}
			b, cleanup := connectAssertFake(t, fake)
			defer cleanup()

			ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 10*time.Second)
			defer cancel()
			res, err := b.ExecuteBatch(ctx, []browser.BatchStep{tc.step})
			if err != nil {
				t.Fatalf("ExecuteBatch: %v", err)
			}
			if len(res.Steps) != 1 {
				t.Fatalf("expected one step result, got %+v", res.Steps)
			}
			step := res.Steps[0]
			if strings.Contains(step.Error, "unknown action") {
				t.Fatalf("bridge does not implement the advertised assert step: %s", step.Error)
			}
			if step.OK != tc.wantOK {
				t.Fatalf("step ok = %t (error %q), want %t", step.OK, step.Error, tc.wantOK)
			}
			if tc.wantError != "" && step.Error != tc.wantError {
				t.Fatalf("step error = %q, want %q", step.Error, tc.wantError)
			}
			if asked := fake.questionsAsked(); tc.wantAsked != nil && !reflect.DeepEqual(asked, tc.wantAsked) {
				t.Fatalf("getters asked = %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}
