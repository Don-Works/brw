package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// The tool surface is the only way an agent reaches detached invocation, so each
// verb has to dispatch to the page expression it claims to — a handler wired to
// the wrong builder would poll for a result with a cancel script.
func TestPageToolVerbsDispatchTheirOwnExpressions(t *testing.T) {
	const id = "0a1b2c3d4e5f6071-3"

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
			args: `{"invocation_id":"` + id + `"}`,
			want: snapshot.BuildPageToolResultExpression(id),
		},
		{
			tool: "brw_page_tool_cancel",
			args: `{"invocation_id":"` + id + `"}`,
			want: snapshot.BuildPageToolCancelExpression(id),
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
	// the invoke script. Anything more would mean it waited.
	ctrl := &interactionController{}
	srv := New(ctrl)
	if _, rpcErr := srv.callTool(context.Background(), "brw_call_page_tool",
		json.RawMessage(`{"name":"export_ledger","arguments":{"format":"csv"},"detach":true,"frame":"#widget"}`)); rpcErr != nil {
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
	if ctrl.expression != want {
		t.Fatalf("brw_call_page_tool evaluated\n%s\nwant\n%s", ctrl.expression, want)
	}
	if ctrl.evaluated != 1 {
		t.Fatalf("detached invoke ran %d evaluates, want exactly the start", ctrl.evaluated)
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
// an empty or malformed one would report "lost" for what is really a typo.
func TestPageToolResultRejectsUnusableInvocationIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "missing", args: `{}`, want: "invocation id is required"},
		{name: "malformed", args: `{"invocation_id":"abc"}`, want: "not recognised"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &interactionController{}
			srv := New(ctrl)
			result, rpcErr := srv.callTool(context.Background(), "brw_page_tool_result", json.RawMessage(tc.args))
			if rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			if text := resultText(result); !strings.Contains(text, tc.want) {
				t.Fatalf("answered %q, want it to name %q", text, tc.want)
			}
			if ctrl.evaluated != 0 {
				t.Fatalf("an unusable id ran %d evaluates, want none", ctrl.evaluated)
			}
		})
	}
}

// timeout_ms is clamped rather than trusted: a caller asking for an hour would
// otherwise pin an agent turn to a page that may never answer.
func TestPageToolTimeoutClamping(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   int
		want string
	}{
		{name: "absent uses the default", ms: 0, want: snapshot.DefaultPageToolTimeout.String()},
		{name: "negative means no wait", ms: -1, want: "0s"},
		{name: "over the cap clamps", ms: 60 * 60 * 1000, want: snapshot.MaxPageToolTimeout.String()},
		{name: "in range passes through", ms: 1500, want: "1.5s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageToolTimeout(tc.ms, snapshot.DefaultPageToolTimeout); got.String() != tc.want {
				t.Fatalf("pageToolTimeout(%d) = %s, want %s", tc.ms, got, tc.want)
			}
		})
	}
}
