package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/snapshot"
)

// interactionController records the interaction long tail. It is a separate
// type from recordingController so the bare controller keeps standing in for a
// transport WITHOUT these capabilities, which is what the refusal test needs.
type interactionController struct {
	recordingController
	expression string
	clipboard  browser.ClipboardOptions
	keyDown    browser.KeyHoldOptions
	keyUp      browser.KeyHoldOptions
	pushState  browser.HistoryStateOptions
	focusedRef string
	evaluated  int
}

func (c *interactionController) Evaluate(_ context.Context, expression string) (any, error) {
	c.expression = expression
	c.evaluated++
	return map[string]any{"value": "stub"}, nil
}

func (c *interactionController) Clipboard(_ context.Context, opts browser.ClipboardOptions) (browser.ClipboardResult, error) {
	c.clipboard = opts
	return browser.ClipboardResult{OK: true, Action: opts.Action, Text: "clip"}, nil
}

func (c *interactionController) KeyDown(_ context.Context, opts browser.KeyHoldOptions) (browser.KeyHoldResult, error) {
	c.keyDown = opts
	return browser.KeyHoldResult{ActionResult: browser.ActionResult{OK: true}, Key: opts.Key, Held: []string{opts.Key}}, nil
}

func (c *interactionController) KeyUp(_ context.Context, opts browser.KeyHoldOptions) (browser.KeyHoldResult, error) {
	c.keyUp = opts
	return browser.KeyHoldResult{ActionResult: browser.ActionResult{OK: true}, Key: opts.Key}, nil
}

func (c *interactionController) PushState(_ context.Context, opts browser.HistoryStateOptions) (browser.HistoryStateResult, error) {
	c.pushState = opts
	return browser.HistoryStateResult{OK: true, URL: "https://corp.example.com" + opts.URL}, nil
}

func (c *interactionController) Focus(_ context.Context, ref string) (browser.ActionResult, error) {
	c.focusedRef = ref
	return browser.ActionResult{OK: true, Message: "focused " + ref, Focus: ref}, nil
}

func callInteraction(t *testing.T, ctrl browser.Controller, tool, args string) string {
	t.Helper()
	srv := New(ctrl)
	result, rpcErr := srv.callTool(context.Background(), tool, json.RawMessage(args))
	if rpcErr != nil {
		t.Fatalf("%s rpc error: %+v", tool, rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestInteractionToolsForwardToTheController(t *testing.T) {
	ctrl := &interactionController{}
	srv := New(ctrl)

	for _, tc := range []struct {
		tool  string
		args  string
		check func(t *testing.T)
	}{
		{tool: "brw_key_down", args: `{"key":"Shift"}`, check: func(t *testing.T) {
			if ctrl.keyDown.Key != "Shift" {
				t.Fatalf("key_down = %+v", ctrl.keyDown)
			}
		}},
		{tool: "brw_key_up", args: `{"key":"all"}`, check: func(t *testing.T) {
			if ctrl.keyUp.Key != "all" {
				t.Fatalf("key_up = %+v", ctrl.keyUp)
			}
		}},
		{tool: "brw_clipboard", args: `{"action":"write","text":"copy me"}`, check: func(t *testing.T) {
			if ctrl.clipboard.Action != "write" || ctrl.clipboard.Text != "copy me" {
				t.Fatalf("clipboard = %+v", ctrl.clipboard)
			}
		}},
		{tool: "brw_pushstate", args: `{"url":"/settings","notify":false}`, check: func(t *testing.T) {
			if ctrl.pushState.URL != "/settings" || ctrl.pushState.Notify == nil || *ctrl.pushState.Notify {
				t.Fatalf("pushstate = %+v", ctrl.pushState)
			}
		}},
		{tool: "brw_focus", args: `{"ref":"e12"}`, check: func(t *testing.T) {
			if ctrl.focusedRef != "e12" {
				t.Fatalf("focused ref = %q", ctrl.focusedRef)
			}
		}},
		{tool: "brw_frame", args: `{"target":"#embed"}`, check: func(t *testing.T) {
			if ctrl.expression != snapshot.BuildFrameSwitchExpression("#embed") {
				t.Fatal("brw_frame evaluated the wrong expression")
			}
		}},
		{tool: "brw_get", args: `{"what":"attr","target":"#name","name":"data-kind"}`, check: func(t *testing.T) {
			want := snapshot.GetRequest{What: "attr", Target: "#name", Name: "data-kind"}.Expression()
			if ctrl.expression != want {
				t.Fatalf("brw_get evaluated\n%s\nwant\n%s", ctrl.expression, want)
			}
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			result, rpcErr := srv.callTool(context.Background(), tc.tool, json.RawMessage(tc.args))
			if rpcErr != nil {
				t.Fatalf("rpc error: %+v", rpcErr)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"isError":true`) {
				t.Fatalf("%s returned an error: %s", tc.tool, encoded)
			}
			tc.check(t)
		})
	}
}

// A transport without a capability must say which capability is missing, not
// fail somewhere downstream with an opaque error.
func TestInteractionToolsNameTheMissingCapability(t *testing.T) {
	ctrl := &recordingController{}
	for _, tc := range []struct{ tool, args, want string }{
		{tool: "brw_key_down", args: `{"key":"Shift"}`, want: "held keys"},
		{tool: "brw_key_up", args: `{"key":"Shift"}`, want: "held keys"},
		{tool: "brw_clipboard", args: `{"action":"read"}`, want: "clipboard"},
		{tool: "brw_pushstate", args: `{"url":"/x"}`, want: "history"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			response := callInteraction(t, ctrl, tc.tool, tc.args)
			if !strings.Contains(response, `"isError":true`) {
				t.Fatalf("%s should refuse without the capability; got %s", tc.tool, response)
			}
			if !strings.Contains(response, tc.want) {
				t.Fatalf("%s refusal should name the capability (%q); got %s", tc.tool, tc.want, response)
			}
		})
	}
}

// The extension bridge cannot run any of these, so advertising them there would
// cost an agent a round trip to discover a capability that was never present.
func TestInteractionToolsHiddenOnTheExtensionBridge(t *testing.T) {
	srv := NewWithToolProfile(nil, "all")
	srv.SetIdentity(brwidentity.Identity{Transport: brwidentity.TransportExtensionBridge})
	advertised := advertisedNames(srv)
	for _, name := range []string{"brw_clipboard", "brw_key_down", "brw_key_up", "brw_pushstate"} {
		if advertised[name] {
			t.Errorf("extension bridge advertised %s, which always fails there", name)
		}
	}
	// The two that run in the page work on both transports.
	for _, name := range []string{"brw_get", "brw_frame", "brw_focus"} {
		if !advertised[name] {
			t.Errorf("extension bridge dropped %s, which works there", name)
		}
	}
}

func TestPushStateToolAppliesNavigationPolicy(t *testing.T) {
	ctrl := &interactionController{}
	srv := New(ctrl)
	srv.SetNavigationPolicy(navpolicy.Parse("corp.example.com", ""))

	result, rpcErr := srv.callTool(context.Background(), "brw_pushstate", json.RawMessage(`{"url":"https://evil.example/x"}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"isError":true`) {
		t.Fatalf("off-allowlist pushstate must be refused; got %s", encoded)
	}
	if ctrl.pushState.URL != "" {
		t.Fatalf("an off-allowlist pushstate still reached the controller: %+v", ctrl.pushState)
	}

	if response := callInteraction(t, ctrl, "brw_pushstate", `{"url":"/settings"}`); strings.Contains(response, `"isError":true`) {
		t.Fatalf("a same-origin route must pass: %s", response)
	}
}

func TestGetToolValidatesBeforeTouchingTheBrowser(t *testing.T) {
	ctrl := &interactionController{}
	srv := New(ctrl)
	result, rpcErr := srv.callTool(context.Background(), "brw_get", json.RawMessage(`{"what":"count"}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "requires target") {
		t.Fatalf("brw_get count with no target should explain what is missing; got %s", encoded)
	}
	if ctrl.evaluated != 0 {
		t.Fatal("an invalid brw_get still reached the browser")
	}
}
