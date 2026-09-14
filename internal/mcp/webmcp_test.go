package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// pageToolStubID is shaped like a real invocation id (<document nonce>-<seq>) so
// it survives validatePageToolID the way one minted in a page would.
const pageToolStubID = "0a1b2c3d4e5f6071-3"

// pageToolController answers the page-tool expressions the way a document does:
// the start script mints an id and reports the tool running, and the poll script
// keeps reporting running until settleAfter polls have gone by. A controller that
// answers anything else makes InvokePageTool return at the start report, which is
// why a detach assertion against a generic stub can never tell detached from
// waited — it never reaches the branch.
type pageToolController struct {
	recordingController

	mu          sync.Mutex
	expressions []string
	labels      []browser.TraceLabel
	polls       int

	// settleAfter is how many polls report running before one reports done;
	// zero keeps the invocation running forever.
	settleAfter int
	result      any
	activeTab   string
}

func (c *pageToolController) ResolveActiveTabID(context.Context) string { return c.activeTab }

func (c *pageToolController) Evaluate(ctx context.Context, expression string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expressions = append(c.expressions, expression)
	label, _ := browser.TraceLabelFromCtx(ctx)
	c.labels = append(c.labels, label)

	switch {
	case strings.Contains(expression, "var ARGS = "):
		return map[string]any{"ok": true, "id": pageToolStubID, "name": "export_ledger", "status": "running", "frame": "main"}, nil
	case strings.Contains(expression, "cancelled by the agent"):
		return map[string]any{"ok": false, "id": pageToolStubID, "status": "cancelled", "cancelled": true}, nil
	default:
		c.polls++
		if c.settleAfter > 0 && c.polls >= c.settleAfter {
			return map[string]any{"ok": true, "id": pageToolStubID, "status": "done", "result": c.result}, nil
		}
		return map[string]any{"ok": false, "id": pageToolStubID, "status": "running"}, nil
	}
}

func (c *pageToolController) evaluates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.expressions)
}

func (c *pageToolController) firstExpression() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.expressions) == 0 {
		return ""
	}
	return c.expressions[0]
}

func (c *pageToolController) firstLabel() browser.TraceLabel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.labels) == 0 {
		return browser.TraceLabel{}
	}
	return c.labels[0]
}

// The tool surface is the only way an agent reaches detached invocation, so each
// verb has to dispatch to the page expression it claims to — a handler wired to
// the wrong builder would poll for a result with a cancel script.
func TestPageToolVerbsDispatchTheirOwnExpressions(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args string
		want string
	}{
		{
			tool: "brw_page_tools",
			args: `{"frame":"#widget"}`,
			want: snapshot.BuildPageToolsExpression("#widget"),
		},
		{
			tool: "brw_page_tool_result",
			args: `{"invocation_id":"` + pageToolStubID + `"}`,
			want: snapshot.BuildPageToolResultExpression(pageToolStubID),
		},
		{
			tool: "brw_page_tool_cancel",
			args: `{"invocation_id":"` + pageToolStubID + `"}`,
			want: snapshot.BuildPageToolCancelExpression(pageToolStubID),
		},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ctrl := &interactionController{}
			srv := New(ctrl)
			if _, rpcErr := srv.callTool(context.Background(), tc.tool, json.RawMessage(tc.args)); rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			if ctrl.expression != tc.want {
				t.Fatalf("%s evaluated\n%s\nwant\n%s", tc.tool, ctrl.expression, tc.want)
			}
		})
	}

	// A detached invoke starts the tool and stops there: one evaluate, and it is
	// the invoke script. The controller reports the invocation running and never
	// settles it, so a build that ignored detach would keep polling to timeout_ms
	// and record more than one.
	ctrl := &pageToolController{}
	srv := New(ctrl)
	if _, rpcErr := srv.callTool(context.Background(), "brw_call_page_tool",
		json.RawMessage(`{"name":"export_ledger","arguments":{"format":"csv"},"detach":true,"frame":"#widget","timeout_ms":300}`)); rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	want, err := snapshot.BuildPageToolInvokeExpression(snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: json.RawMessage(`{"format":"csv"}`),
		Frame:     "#widget",
		Detach:    true,
		Validate:  true,
	})
	if err != nil {
		t.Fatalf("build expected expression: %v", err)
	}
	if got := ctrl.firstExpression(); got != want {
		t.Fatalf("brw_call_page_tool evaluated\n%s\nwant\n%s", got, want)
	}
	if got := ctrl.evaluates(); got != 1 {
		t.Fatalf("detached invoke ran %d evaluates, want exactly the start", got)
	}
}

// validate_input defaults on, and turning it off has to actually reach the page
// script rather than being read and dropped.
func TestPageToolValidationIsOnUnlessTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     string
		validate bool
	}{
		{name: "default", args: `{"name":"t","arguments":{}}`, validate: true},
		{name: "explicit off", args: `{"name":"t","arguments":{},"validate_input":false}`, validate: false},
		{name: "explicit on", args: `{"name":"t","arguments":{},"validate_input":true}`, validate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &interactionController{}
			srv := New(ctrl)
			if _, rpcErr := srv.callTool(context.Background(), "brw_call_page_tool", json.RawMessage(tc.args)); rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			want, err := snapshot.BuildPageToolInvokeExpression(snapshot.PageToolInvokeOptions{
				Name:      "t",
				Arguments: json.RawMessage(`{}`),
				Validate:  tc.validate,
			})
			if err != nil {
				t.Fatalf("build expected expression: %v", err)
			}
			if ctrl.expression != want {
				t.Fatalf("validate_input %v did not reach the page script", tc.validate)
			}
		})
	}
}

// The input cap is refused at the tool boundary with a named error, and nothing
// is evaluated: an oversized payload never becomes an expression at all.
func TestOversizedPageToolArgumentsNeverReachTheTransport(t *testing.T) {
	ctrl := &interactionController{}
	srv := New(ctrl)

	oversized, err := json.Marshal(map[string]any{
		"name":      "export_ledger",
		"arguments": map[string]string{"blob": strings.Repeat("x", snapshot.MaxPageToolInputBytes)},
	})
	if err != nil {
		t.Fatalf("build args: %v", err)
	}
	result, rpcErr := srv.callTool(context.Background(), "brw_call_page_tool", oversized)
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	text := resultText(result)
	if !strings.Contains(text, "webmcp page tool input too large") {
		t.Fatalf("oversized call answered %q, want the named cap error", text)
	}
	if ctrl.evaluated != 0 {
		t.Fatalf("oversized call ran %d evaluates, want none", ctrl.evaluated)
	}
}

// An invocation id is a handle on work running in a page; a tool that accepted
// an empty or malformed one would report "lost" for what is really a typo. The
// only spelling of the parameter is the advertised one: an undocumented alias is
// a second contract nothing keeps in step with the schema.
func TestPageToolResultRejectsUnusableInvocationIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "missing", args: `{}`, want: "invocation id is required"},
		{name: "malformed", args: `{"invocation_id":"abc"}`, want: "not recognised"},
		{name: "unadvertised id alias", args: `{"id":"` + pageToolStubID + `"}`, want: "invocation id is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, tool := range []string{"brw_page_tool_result", "brw_page_tool_cancel"} {
				ctrl := &interactionController{}
				srv := New(ctrl)
				result, rpcErr := srv.callTool(context.Background(), tool, json.RawMessage(tc.args))
				if rpcErr != nil {
					t.Fatalf("%s rpc error: %+v", tool, rpcErr)
				}
				if text := resultText(result); !strings.Contains(text, tc.want) {
					t.Fatalf("%s answered %q, want it to name %q", tool, text, tc.want)
				}
				if ctrl.evaluated != 0 {
					t.Fatalf("%s ran %d evaluates for an unusable id, want none", tool, ctrl.evaluated)
				}
			}
		})
	}
}

// timeout_ms is clamped rather than trusted: a caller asking for an hour would
// otherwise pin an agent turn to a page that may never answer. A negative value
// has no single sensible reading — no wait, or the default wait, are thirty
// seconds apart — so it is refused instead of guessed at.
func TestPageToolTimeoutClamping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ms      int
		want    string
		wantErr bool
	}{
		{name: "absent uses the default", ms: 0, want: snapshot.DefaultPageToolTimeout.String()},
		{name: "negative is refused", ms: -1, wantErr: true},
		{name: "over the cap clamps", ms: 60 * 60 * 1000, want: snapshot.MaxPageToolTimeout.String()},
		{name: "in range passes through", ms: 1500, want: "1.5s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pageToolTimeout(tc.ms, snapshot.DefaultPageToolTimeout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pageToolTimeout(%d) = %s with no error, want it refused", tc.ms, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pageToolTimeout(%d): %v", tc.ms, err)
			}
			if got.String() != tc.want {
				t.Fatalf("pageToolTimeout(%d) = %s, want %s", tc.ms, got, tc.want)
			}
		})
	}
}

// A refused timeout has to be refused at the boundary, not turned into a wait of
// some other length: a negative timeout_ms once meant "block for the default 30
// seconds" on one verb and "one cheap poll" on the other.
func TestNegativePageToolTimeoutIsRefusedBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args string
	}{
		{tool: "brw_call_page_tool", args: `{"name":"export_ledger","timeout_ms":-1}`},
		{tool: "brw_page_tool_result", args: `{"invocation_id":"` + pageToolStubID + `","timeout_ms":-1}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ctrl := &pageToolController{}
			srv := New(ctrl)
			result, rpcErr := srv.callTool(context.Background(), tc.tool, json.RawMessage(tc.args))
			if rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			if text := resultText(result); !strings.Contains(text, "timeout_ms must not be negative") {
				t.Fatalf("answered %q, want the negative timeout refused by name", text)
			}
			if got := ctrl.evaluates(); got != 0 {
				t.Fatalf("a refused timeout ran %d evaluates, want none", got)
			}
		})
	}
}

// A poll walks only the windows of the tab it lands in, and a page tool that
// opens a tab moves the active one. The report has to name the tab it was
// started in, because the agent has nothing else to pass back.
func TestPageToolReportNamesTheTabToPollBackInto(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tool      string
		args      string
		activeTab string
		want      string
	}{
		{
			name:      "start reports the resolved active tab",
			tool:      "brw_call_page_tool",
			args:      `{"name":"export_ledger","arguments":{"format":"csv"},"detach":true}`,
			activeTab: "tab-7",
			want:      `"tab_id":"tab-7"`,
		},
		{
			name:      "an explicit tab_id is echoed back",
			tool:      "brw_call_page_tool",
			args:      `{"name":"export_ledger","arguments":{"format":"csv"},"detach":true,"tab_id":"tab-9"}`,
			activeTab: "tab-7",
			want:      `"tab_id":"tab-9"`,
		},
		{
			name:      "a poll reports the tab it polled",
			tool:      "brw_page_tool_result",
			args:      `{"invocation_id":"` + pageToolStubID + `"}`,
			activeTab: "tab-7",
			want:      `"tab_id":"tab-7"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &pageToolController{activeTab: tc.activeTab}
			srv := New(ctrl)
			result, rpcErr := srv.callTool(context.Background(), tc.tool, json.RawMessage(tc.args))
			if rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			if text := resultText(result); !strings.Contains(text, tc.want) {
				t.Fatalf("answered %q, want it to carry %s", text, tc.want)
			}
		})
	}
}

// A page tool's return value is written by the page, so its size is the page's
// choice. It goes through the same windowing as brw_evaluate rather than being
// serialised whole into the agent's turn — and a finished result stays
// collectable for five minutes, so an unbounded one could be re-dumped at will.
func TestPageToolResultIsBoundedLikeEvaluate(t *testing.T) {
	huge := strings.Repeat("y", 100000)

	t.Run("default caps and marks the truncation", func(t *testing.T) {
		ctrl := &pageToolController{settleAfter: 1, result: huge}
		srv := New(ctrl)
		result, rpcErr := srv.callTool(context.Background(), "brw_page_tool_result",
			json.RawMessage(`{"invocation_id":"`+pageToolStubID+`"}`))
		if rpcErr != nil {
			t.Fatalf("rpc error: %+v", rpcErr)
		}
		text := resultText(result)
		if !strings.Contains(text, "truncated: returned") {
			t.Fatalf("a %d byte page tool result came back unmarked: %.200s", len(huge), text)
		}
		if len(text) > 2*defaultEvaluateMaxBytes {
			t.Fatalf("result text is %d bytes, want it bounded near %d", len(text), defaultEvaluateMaxBytes)
		}
	})

	t.Run("max_bytes windows it", func(t *testing.T) {
		ctrl := &pageToolController{settleAfter: 1, result: huge}
		srv := New(ctrl)
		result, rpcErr := srv.callTool(context.Background(), "brw_page_tool_result",
			json.RawMessage(`{"invocation_id":"`+pageToolStubID+`","max_bytes":200}`))
		if rpcErr != nil {
			t.Fatalf("rpc error: %+v", rpcErr)
		}
		text := resultText(result)
		if !strings.Contains(text, "truncated: returned 200 of") {
			t.Fatalf("max_bytes=200 answered %.300s, want a 200 byte window", text)
		}
	})

	t.Run("a big result from the call itself is bounded too", func(t *testing.T) {
		ctrl := &pageToolController{settleAfter: 1, result: huge}
		srv := New(ctrl)
		result, rpcErr := srv.callTool(context.Background(), "brw_call_page_tool",
			json.RawMessage(`{"name":"export_ledger","arguments":{"format":"csv"},"max_bytes":200}`))
		if rpcErr != nil {
			t.Fatalf("rpc error: %+v", rpcErr)
		}
		if text := resultText(result); !strings.Contains(text, "truncated: returned 200 of") {
			t.Fatalf("brw_call_page_tool answered %.300s, want a 200 byte window", text)
		}
	})
}

// Every poll is a full Evaluate, and a wait at the ten-minute cap runs close to
// six hundred of them. Recorded as raw evaluate rows they would evict the whole
// 500-entry trace ring, so each one is labelled by the verb that ran it and
// collapses into a single row.
func TestPageToolEvaluatesAreLabelledForTheTrace(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args string
		want browser.TraceLabel
	}{
		{
			tool: "brw_call_page_tool",
			args: `{"name":"export_ledger","arguments":{"format":"csv"},"detach":true}`,
			want: browser.TraceLabel{Action: browser.TraceActionPageTool, Value: "call export_ledger"},
		},
		{
			tool: "brw_page_tool_result",
			args: `{"invocation_id":"` + pageToolStubID + `"}`,
			want: browser.TraceLabel{Action: browser.TraceActionPageTool, Value: "result " + pageToolStubID},
		},
		{
			tool: "brw_page_tool_cancel",
			args: `{"invocation_id":"` + pageToolStubID + `"}`,
			want: browser.TraceLabel{Action: browser.TraceActionPageTool, Value: "cancel " + pageToolStubID},
		},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ctrl := &pageToolController{}
			srv := New(ctrl)
			if _, rpcErr := srv.callTool(context.Background(), tc.tool, json.RawMessage(tc.args)); rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			if got := ctrl.firstLabel(); got != tc.want {
				t.Fatalf("%s labelled its evaluate %+v, want %+v", tc.tool, got, tc.want)
			}
		})
	}
}

// A wait can end without the invocation ending: the request is cancelled, the
// tab closes, an evaluate fails. The tool description promises the invocation is
// never abandoned, only stopped being waited on, so the id has to come back in
// the report rather than only inside an error string.
func TestInterruptedPageToolWaitKeepsTheInvocationAddressable(t *testing.T) {
	ctrl := &pageToolController{activeTab: "tab-7"}
	srv := New(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	result, rpcErr := srv.callTool(ctx, "brw_page_tool_result",
		json.RawMessage(`{"invocation_id":"`+pageToolStubID+`","timeout_ms":30000}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	text := resultText(result)
	for _, want := range []string{pageToolStubID, `"interrupted":true`, "brw_page_tool_cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("interrupted wait answered %q, want it to carry %q", text, want)
		}
	}
}
