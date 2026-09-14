package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const getterFixture = `<!doctype html><html><head><title>Getter fixture</title></head><body>
<h1 id="heading">Hello</h1>
<input id="name" value="Ada" data-kind="person">
<input id="agree" type="checkbox" checked>
<input id="frozen" value="fixed" readonly>
<div id="editor" contenteditable="true">notes</div>
<button id="go" disabled>Go</button>
<p class="row">one</p><p class="row">two</p><p class="row">three</p>
<div id="gone" style="display:none">invisible</div>
<iframe srcdoc="<p id='inframe'>inside the frame</p>"></iframe>
<script>document.getElementById('name').focus();</script>
</body></html>`

func evalJSON(t *testing.T, ctx context.Context, expr string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).WithReturnByValue(true).Do(c)
		if err != nil {
			return err
		}
		if exception != nil {
			if exception.Exception != nil && exception.Exception.Description != "" {
				return fmt.Errorf("%s", exception.Exception.Description)
			}
			return fmt.Errorf("exception")
		}
		return json.Unmarshal(obj.Value, &out)
	})); err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	return out
}

func TestGetScript(t *testing.T) {
	tests := []struct {
		name   string
		what   string
		target string
		attr   string
		want   any
	}{
		{name: "title", what: "title", want: "Getter fixture"},
		{name: "element text", what: "text", target: "#heading", want: "Hello"},
		{name: "input value", what: "value", target: "#name", want: "Ada"},
		{name: "attribute", what: "attr", target: "#name", attr: "data-kind", want: "person"},
		{name: "count matches every element", what: "count", target: ".row", want: float64(3)},
		{name: "visible element", what: "visible", target: "#heading", want: true},
		{name: "display:none is not visible", what: "visible", target: "#gone", want: false},
		{name: "hidden is the inverse", what: "hidden", target: "#gone", want: true},
		{name: "disabled button", what: "disabled", target: "#go", want: true},
		{name: "enabled input", what: "enabled", target: "#name", want: true},
		{name: "checked box", what: "checked", target: "#agree", want: true},
		{name: "single css property", what: "styles", target: "#gone", attr: "display", want: "none"},
		{name: "editable input", what: "editable", target: "#name", want: true},
		{name: "readonly input is not editable", what: "editable", target: "#frozen", want: false},
		{name: "disabled control is not editable", what: "editable", target: "#go", want: false},
		{name: "contenteditable is editable", what: "editable", target: "#editor", want: true},
		{name: "focused input", what: "focused", target: "#name", want: true},
		{name: "unfocused input", what: "focused", target: "#agree", want: false},
		{name: "navigation http status", what: "status", want: float64(200)},
		// Frame-awareness is the reason this exists rather than a bare
		// document.querySelector in brw_evaluate.
		{name: "resolves inside a same-origin iframe", what: "text", target: "#inframe", want: "inside the frame"},
	}

	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, getterFixture)
	}))
	defer srv.Close()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalJSON(t, ctx, BuildGetExpression(tt.what, tt.target, tt.attr))
			if got["value"] != tt.want {
				t.Fatalf("get %s(%s) = %#v, want %#v", tt.what, tt.target, got["value"], tt.want)
			}
		})
	}
}

// TestGetScriptStateReportsEveryFlagAtOnce covers the 'state' case: one round
// trip for every interaction flag, with found reported separately so a caller
// can tell "disabled" from "not on the page".
func TestGetScriptStateReportsEveryFlagAtOnce(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, getterFixture)
	}))
	defer srv.Close()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	tests := []struct {
		name   string
		target string
		want   map[string]any
	}{
		{name: "focused text input", target: "#name", want: map[string]any{
			"found": true, "visible": true, "enabled": true, "editable": true, "checked": false, "focused": true,
		}},
		{name: "checked box", target: "#agree", want: map[string]any{
			"found": true, "visible": true, "enabled": true, "editable": true, "checked": true, "focused": false,
		}},
		{name: "disabled button", target: "#go", want: map[string]any{
			"found": true, "visible": true, "enabled": false, "editable": false, "checked": false, "focused": false,
		}},
		{name: "missing element", target: "#nope", want: map[string]any{
			"found": false, "visible": false, "enabled": false, "editable": false, "checked": false, "focused": false,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalJSON(t, ctx, BuildGetExpression("state", tt.target, ""))
			state, ok := got["value"].(map[string]any)
			if !ok {
				t.Fatalf("state value = %#v, want an object", got["value"])
			}
			for key, want := range tt.want {
				if state[key] != want {
					t.Fatalf("state[%q] = %#v, want %#v (full: %#v)", key, state[key], want, state)
				}
			}
		})
	}
}

func TestGetScriptBoxHasGeometry(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, getterFixture)
	}))
	defer srv.Close()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got := evalJSON(t, ctx, BuildGetExpression("box", "#heading", ""))
	box, ok := got["value"].(map[string]any)
	if !ok {
		t.Fatalf("box value = %#v, want an object", got["value"])
	}
	width, _ := box["width"].(float64)
	height, _ := box["height"].(float64)
	if width <= 0 || height <= 0 {
		t.Fatalf("box = %#v, want positive width and height", box)
	}
}

func TestStorageScriptRoundTrip(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>storage</body></html>")
	}))
	defer srv.Close()

	for _, kind := range []string{"local", "session"} {
		t.Run(kind, func(t *testing.T) {
			if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
				t.Fatalf("navigate: %v", err)
			}
			evalJSON(t, ctx, BuildStorageExpression(kind, "clear", "", ""))

			set := evalJSON(t, ctx, BuildStorageExpression(kind, "set", "flag", "on"))
			if set["value"] != "on" {
				t.Fatalf("set returned %#v", set)
			}
			got := evalJSON(t, ctx, BuildStorageExpression(kind, "get", "flag", ""))
			if got["value"] != "on" {
				t.Fatalf("get returned %#v, want on", got)
			}
			all := evalJSON(t, ctx, BuildStorageExpression(kind, "get", "", ""))
			items, _ := all["items"].(map[string]any)
			if items["flag"] != "on" {
				t.Fatalf("get-all returned %#v", all)
			}
			evalJSON(t, ctx, BuildStorageExpression(kind, "remove", "flag", ""))
			after := evalJSON(t, ctx, BuildStorageExpression(kind, "get", "flag", ""))
			if after["value"] != nil {
				t.Fatalf("removed key still reads %#v", after["value"])
			}
		})
	}
}
