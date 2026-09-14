package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/devtools/axe"
)

// sentExpressions collects what crossed the socket. The stub writes it from its
// own goroutine and the test reads it from another, and a loopback write/read
// pair orders bytes without ordering memory, so the slice needs a latch of its
// own rather than the socket's apparent sequencing.
type sentExpressions struct {
	mu   chan struct{}
	list []string
}

func newSentExpressions() *sentExpressions {
	s := &sentExpressions{mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	return s
}

func (s *sentExpressions) add(expression string) {
	<-s.mu
	s.list = append(s.list, expression)
	s.mu <- struct{}{}
}

func (s *sentExpressions) all() []string {
	<-s.mu
	out := append([]string(nil), s.list...)
	s.mu <- struct{}{}
	return out
}

// serveEvaluateStub stands in for the extension: it answers every
// Runtime.evaluate the bridge sends with whatever reply the test decides, and
// records the expressions so a test can assert on what actually crossed.
func serveEvaluateStub(t *testing.T, b *Bridge, reply func(expression string) any) (*sentExpressions, func()) {
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
	// The embedded accessibility engine is half a megabyte of expression. A
	// real extension's WebSocket has no receive cap; this client library
	// defaults to 32 KiB and would close the socket as "message too big", so
	// the stub has to be as permissive as the browser it stands in for.
	conn.SetReadLimit(extensionFrameReadLimitBytes)
	waitUntil(t, b.liveConn)

	sent := newSentExpressions()
	done := make(chan struct{})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg request
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			params, _ := msg.Params["params"].(map[string]any)
			expression, _ := params["expression"].(string)
			sent.add(expression)
			answer, _ := json.Marshal(map[string]any{
				"id": msg.ID,
				"ok": true,
				"result": map[string]any{
					"result": map[string]any{"value": reply(expression)},
				},
			})
			_ = conn.Write(serveCtx, websocket.MessageText, answer)
		}
	}()
	return sent, func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
		<-done
	}
}

func bridgeTabContext() context.Context {
	return browser.WithTabID(context.Background(), "42")
}

// TestBridgeVitalsEvaluatesTheSharedScript: the second transport has to send the
// same expression the first one does, or a reading means two different things
// depending on which browser connection an agent happens to have.
func TestBridgeVitalsEvaluatesTheSharedScript(t *testing.T) {
	b := New("", 5*time.Second, "")
	sent, cleanup := serveEvaluateStub(t, b, func(string) any {
		return map[string]any{
			"url": "https://example.test/page", "cls": 0.25, "cls_shifts": 3,
			"ttfb_ms": 180.4, "lcp_ms": 900.1, "lcp_element": "div#hero",
			"ratings": map[string]any{"cls": "poor"}, "settled_ms": 400,
		}
	})
	defer cleanup()

	vitals, err := b.Vitals(bridgeTabContext(), devtools.VitalsOptions{SettleMS: 400})
	if err != nil {
		t.Fatalf("vitals: %v", err)
	}
	if vitals.CLS == nil || *vitals.CLS != 0.25 || vitals.CLSShifts != 3 || vitals.TTFBMS == nil || *vitals.TTFBMS != 180.4 {
		t.Fatalf("decoded vitals = %+v", vitals)
	}
	if vitals.Ratings["cls"] != "poor" {
		t.Errorf("ratings = %v, want the page's own labels carried through", vitals.Ratings)
	}
	if len(sent.all()) != 1 {
		t.Fatalf("bridge sent %d expressions, want exactly one round trip", len(sent.all()))
	}
	if sent.all()[0] != devtools.BuildVitalsExpression(devtools.VitalsOptions{SettleMS: 400}) {
		t.Fatal("the bridge sent an expression the direct-CDP transport does not")
	}
}

// TestBridgeAccessibilityAuditInjectsTheEmbeddedEngine is the no-network claim
// on the transport that drives a real signed-in Chrome: the engine has to cross
// the bridge as an expression, never as something the page fetches.
func TestBridgeAccessibilityAuditInjectsTheEmbeddedEngine(t *testing.T) {
	tests := []struct {
		name         string
		alreadyThere bool
		wantInstall  bool
		wantCalls    int
	}{
		{name: "a document with no engine gets the embedded one", wantInstall: true, wantCalls: 3},
		{name: "a document that already has one is not re-injected", alreadyThere: true, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			sent, cleanup := serveEvaluateStub(t, b, func(expression string) any {
				switch {
				case expression == devtools.AxeProbeScript:
					return map[string]any{"present": tt.alreadyThere}
				case strings.Contains(expression, axe.Source):
					return map[string]any{"installed": true, "version": axe.Version}
				default:
					return map[string]any{
						"ok": true, "ours": true, "engine": "axe-core " + axe.Version,
						"url": "https://example.test/page", "title": "Page",
						"report": map[string]any{
							"violations": []any{map[string]any{
								"id": "color-contrast", "impact": "serious", "help": "Contrast",
								"helpUrl": "https://example.com/contrast", "tags": []any{"wcag2aa"},
								"nodes": []any{map[string]any{"target": []any{"#low"}, "brw_ref": "e9"}},
							}},
							"incomplete": []any{}, "passes": []any{}, "inapplicable": []any{},
						},
					}
				}
			})
			defer cleanup()

			result, err := b.AccessibilityAudit(bridgeTabContext(), devtools.AuditOptions{})
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			if len(result.Rules) != 1 || result.Rules[0].ID != "color-contrast" || result.Rules[0].Refs[0] != "e9" {
				t.Fatalf("summary = %+v", result.Rules)
			}
			if len(result.Report) == 0 {
				t.Fatal("no full report was produced for the artifact")
			}

			if len(sent.all()) != tt.wantCalls {
				t.Fatalf("bridge sent %d expressions, want %d", len(sent.all()), tt.wantCalls)
			}
			injected := false
			for _, expression := range sent.all() {
				if strings.Contains(expression, axe.Source) {
					injected = true
				}
				// Whatever crossed, none of it may ask the page to go and load
				// the engine from somewhere.
				if strings.Contains(expression, "src=") && strings.Contains(expression, "axe.min.js") {
					t.Fatal("the bridge asked the page to load axe over the network")
				}
			}
			if injected != tt.wantInstall {
				t.Fatalf("engine injected = %v, want %v", injected, tt.wantInstall)
			}
		})
	}
}

// TestBridgeHighlightRoundTripsTheOverlay covers the third tool and the
// reversibility it advertises, on the transport an agent uses against a real
// browser window where a human can actually see the overlay.
func TestBridgeHighlightRoundTripsTheOverlay(t *testing.T) {
	b := New("", 5*time.Second, "")
	sent, cleanup := serveEvaluateStub(t, b, func(expression string) any {
		if strings.Contains(expression, `"clear":true`) {
			return map[string]any{"ok": true, "cleared": true, "active": 0, "reversible": "nothing added"}
		}
		return map[string]any{
			"ok":         true,
			"active":     1,
			"marked":     []any{map[string]any{"ref": "e9", "found": true, "in_viewport": true, "width": 120.0, "height": 20.0}},
			"reversible": `one <div id="` + devtools.HighlightHostID + `"> was added`,
		}
	})
	defer cleanup()

	marked, err := b.Highlight(bridgeTabContext(), devtools.HighlightOptions{Ref: "e9", Color: "green"})
	if err != nil {
		t.Fatalf("highlight: %v", err)
	}
	if marked.Active != 1 || len(marked.Marked) != 1 || !marked.Marked[0].Found {
		t.Fatalf("highlight result = %+v", marked)
	}
	if !strings.Contains(marked.Reversible, devtools.HighlightHostID) {
		t.Errorf("reversible = %q, want it to name what was added", marked.Reversible)
	}

	cleared, err := b.Highlight(bridgeTabContext(), devtools.HighlightOptions{Clear: true})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !cleared.Cleared || cleared.Active != 0 {
		t.Fatalf("clear result = %+v", cleared)
	}
	if len(sent.all()) != 2 {
		t.Fatalf("bridge sent %d expressions, want one per call", len(sent.all()))
	}
	// A colour is a value written into an inline style, so it must reach the
	// page as the resolved hex from the closed set, never as caller text.
	if strings.Contains(sent.all()[0], "green") {
		t.Error("the colour name was passed into the page instead of the resolved value")
	}
}

// TestBridgeHighlightRefusesBadArgumentsBeforeTheRoundTrip: validation belongs
// on both transports, not only in whichever one a test happened to cover.
func TestBridgeHighlightRefusesBadArgumentsBeforeTheRoundTrip(t *testing.T) {
	b := New("", 5*time.Second, "")
	sent, cleanup := serveEvaluateStub(t, b, func(string) any { return map[string]any{"ok": true} })
	defer cleanup()

	if _, err := b.Highlight(bridgeTabContext(), devtools.HighlightOptions{}); err == nil ||
		!strings.Contains(err.Error(), "at least one ref") {
		t.Fatalf("error = %v, want a refusal naming the missing ref", err)
	}
	if len(sent.all()) != 0 {
		t.Fatalf("bridge sent %d expressions for an invalid call, want none", len(sent.all()))
	}
}

// TestBridgeRefusesAnEmptyObservation: the generic evaluate path turns an
// undefined completion value into JSON null on purpose, because a page script
// that returns nothing is a successful evaluation. These three expressions
// always resolve to an object, so the same null means the script never ran —
// and a zero Vitals reads exactly like a clean page.
func TestBridgeRefusesAnEmptyObservation(t *testing.T) {
	tests := []struct {
		name string
		call func(*Bridge) error
	}{
		{
			name: "vitals",
			call: func(b *Bridge) error {
				_, err := b.Vitals(bridgeTabContext(), devtools.VitalsOptions{})
				return err
			},
		},
		{
			name: "highlight",
			call: func(b *Bridge) error {
				_, err := b.Highlight(bridgeTabContext(), devtools.HighlightOptions{Ref: "e1"})
				return err
			},
		},
		{
			name: "the audit, at the probe that decides whether to inject",
			call: func(b *Bridge) error {
				_, err := b.AccessibilityAudit(bridgeTabContext(), devtools.AuditOptions{})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			_, cleanup := serveEvaluateStub(t, b, func(string) any { return nil })
			defer cleanup()

			err := tt.call(b)
			if !errors.Is(err, devtools.ErrNoObservation) {
				t.Fatalf("error = %v, want the named empty-observation error", err)
			}
		})
	}
}

// TestBridgeRecordsTheDevtoolsActionsInTheTrace is the parity half of the
// direct-CDP trace test: a human watching a bridged daemon has to see the same
// three entries, or "what did brw do" depends on which transport answered.
func TestBridgeRecordsTheDevtoolsActionsInTheTrace(t *testing.T) {
	b := New("", 5*time.Second, "")
	_, cleanup := serveEvaluateStub(t, b, func(expression string) any {
		switch {
		case expression == devtools.AxeProbeScript:
			return map[string]any{"present": true}
		case strings.Contains(expression, devtools.HighlightScript):
			return map[string]any{"ok": true, "active": 1,
				"marked": []any{map[string]any{"ref": "e9", "found": true}}}
		case strings.Contains(expression, devtools.VitalsScript):
			return map[string]any{"url": "https://example.test/page", "settled_ms": 250}
		default:
			return map[string]any{
				"ok": true, "ours": true, "engine": "axe-core " + axe.Version,
				"url": "https://example.test/page", "title": "Page",
				"report": map[string]any{"violations": []any{}, "incomplete": []any{}, "passes": []any{}, "inapplicable": []any{}},
			}
		}
	})
	defer cleanup()

	if _, err := b.Vitals(bridgeTabContext(), devtools.VitalsOptions{}); err != nil {
		t.Fatalf("vitals: %v", err)
	}
	if _, err := b.AccessibilityAudit(bridgeTabContext(), devtools.AuditOptions{}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if _, err := b.Highlight(bridgeTabContext(), devtools.HighlightOptions{Ref: "e9", Label: "look here"}); err != nil {
		t.Fatalf("highlight: %v", err)
	}

	entries := b.GetTrace().Entries
	byAction := map[string]browser.TraceEntry{}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		byAction[entry.Action] = entry
		actions = append(actions, entry.Action)
	}
	for _, action := range []string{browser.TraceActionVitals, browser.TraceActionAudit, browser.TraceActionHighlight} {
		entry, ok := byAction[action]
		if !ok {
			t.Fatalf("no %q entry in the bridge trace; it recorded %v", action, actions)
		}
		if entry.TabID != "42" {
			t.Errorf("%q recorded tab %q, want the bridged tab", action, entry.TabID)
		}
		if !entry.OK {
			t.Errorf("%q recorded as failed: %q", action, entry.Error)
		}
	}
	if ref := byAction[browser.TraceActionHighlight].Ref; ref != "e9" {
		t.Errorf("highlight recorded ref %q, want e9", ref)
	}
	// The caption is caller text and the trace is served over the control plane.
	if strings.Contains(byAction[browser.TraceActionHighlight].Text, "look here") {
		t.Errorf("the highlight caption reached the trace: %q", byAction[browser.TraceActionHighlight].Text)
	}
}
