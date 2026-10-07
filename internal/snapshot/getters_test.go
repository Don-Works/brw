package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const getterFixture = `<!doctype html><html><head><title>Getter fixture</title></head><body>
<h1 id="heading">Hello</h1>
<input id="name" value="Ada" data-kind="person">
<input id="empty" value="">
<input id="agree" type="checkbox" checked>
<input id="frozen" value="fixed" readonly>
<input id="password" type="password" value="fixture-password-value" aria-label="Password">
<input id="hidden" type="hidden" value="fixture-hidden-value">
<input id="otp" autocomplete="one-time-code" value="fixture-otp-value">
<input id="card" autocomplete="section-billing cc-number" value="fixture-card-value">
<textarea id="password-text" autocomplete="current-password">fixture-textarea-password</textarea>
<select id="card-select" autocomplete="cc-type"><option value="fixture-card-select" selected>Fixture card</option></select>
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
		name      string
		what      string
		target    string
		attr      string
		want      any
		sensitive bool
	}{
		{name: "title", what: "title", want: "Getter fixture"},
		{name: "element text", what: "text", target: "#heading", want: "Hello"},
		{name: "input value", what: "value", target: "#name", want: "Ada"},
		{name: "ordinary empty value remains exact", what: "value", target: "#empty", want: ""},
		{name: "password value withheld", what: "value", target: "#password", want: "", sensitive: true},
		{name: "hidden value withheld", what: "value", target: "#hidden", want: "", sensitive: true},
		{name: "one-time code withheld", what: "value", target: "#otp", want: "", sensitive: true},
		{name: "payment number withheld", what: "value", target: "#card", want: "", sensitive: true},
		{name: "password attribute withheld", what: "attr", target: "#password", attr: "VALUE", want: "", sensitive: true},
		{name: "password textarea text withheld", what: "text", target: "#password-text", want: "", sensitive: true},
		{name: "password textarea value withheld", what: "value", target: "#password-text", want: "", sensitive: true},
		{name: "payment select value withheld", what: "value", target: "#card-select", want: "", sensitive: true},
		{name: "sensitive field accessible label remains visible", what: "attr", target: "#password", attr: "aria-label", want: "Password"},
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
			if sensitive, _ := got["sensitive"].(bool); sensitive != tt.sensitive {
				t.Fatalf("get %s(%s) sensitive=%t, want %t", tt.what, tt.target, sensitive, tt.sensitive)
			}
		})
	}
}

func TestSensitiveValueAssertionsOnlyReturnTheComparison(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, getterFixture)
	}))
	defer server.Close()
	if err := chromedp.Run(ctx, chromedp.Navigate(server.URL), chromedp.Evaluate(`document.getElementById('password').setAttribute('data-brw-ref','sensitive-fixture')`, nil)); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ script, expected string }{{AssertValueScript, "fixture-password-value"}, {AssertValueContainsScript, "password-value"}} {
		if err := EvalAssert(ctx, check.script, "sensitive-fixture", check.expected, int64(1)); err != nil {
			t.Fatalf("private-value comparison failed: %v", err)
		}
		if err := EvalAssert(ctx, check.script, "sensitive-fixture", "wrong-fixture-value", int64(1)); !errors.Is(err, ErrAssertionTimeout) || strings.Contains(err.Error(), "fixture-password-value") {
			t.Fatalf("failed comparison disclosed an actual value or lost timeout semantics: %v", err)
		}
	}
}

func TestAssertionsReleaseTheirMutationObservers(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML='<input id="assertion-fixture" data-brw-ref="fixture-assertion">';
window.fixtureObservers={active:0,created:0};
window.MutationObserver=class extends MutationObserver {
  constructor(fn){ super(fn); fixtureObservers.created++; }
  observe(...args){ super.observe(...args); if(!this.fixtureConnected){this.fixtureConnected=true;fixtureObservers.active++;} }
  disconnect(){super.disconnect(); if(this.fixtureConnected){this.fixtureConnected=false;fixtureObservers.active--;}}
};`, nil)); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ name, script, reset, change, args string }{
		{"text", AssertTextScript, `el.value='before'`, `el.value='expected';el.setAttribute('data-changed','yes')`, `'fixture-assertion','expected',5`},
		{"value", AssertValueScript, `el.value='before'`, `el.value='expected';el.setAttribute('data-changed','yes')`, `'fixture-assertion','expected',5`},
		{"contains", AssertValueContainsScript, `el.value='before'`, `el.value='an expected value';el.setAttribute('data-changed','yes')`, `'fixture-assertion','expected',5`},
		{"visible", AssertVisibleScript, `el.hidden=true`, `el.hidden=false`, `'fixture-assertion',5`},
		{"hidden", AssertHiddenScript, `el.hidden=false`, `el.hidden=true`, `'fixture-assertion',5`},
	} {
		t.Run(check.name, func(t *testing.T) {
			for _, phase := range []string{"timeout", "mutation", "immediate"} {
				body := `var el=document.getElementById('assertion-fixture');el.removeAttribute('data-changed');` + check.reset + `;`
				if phase == "immediate" {
					body += check.change + `;`
				}
				body += `var promise=(` + check.script + `)(` + check.args + `);`
				if phase == "mutation" {
					body += check.change + `;`
				}
				body += `return promise;`
				err := EvalAssert(ctx, `(function(){`+body+`})`)
				if phase == "timeout" {
					if !errors.Is(err, ErrAssertionTimeout) {
						t.Fatalf("%s: %v", phase, err)
					}
				} else if err != nil {
					t.Fatalf("%s: %v", phase, err)
				}
				stats := evalJSON(t, ctx, `fixtureObservers`)
				if stats["active"] != float64(0) {
					t.Fatalf("%s left %v connected observers", phase, stats["active"])
				}
			}
		})
	}
	for _, phase := range []string{"mutation", "interval", "timeout"} {
		t.Run("throw after registration/"+phase, func(t *testing.T) {
			timeoutMS := 5
			if phase == "interval" {
				timeoutMS = 200
			}
			mutation := ""
			if phase == "mutation" {
				mutation = `el.setAttribute('data-thrown','yes');`
			}
			script := fmt.Sprintf(`(function(){
var el=document.getElementById('assertion-fixture');delete el.value;el.value='before';
var promise=(%s)('fixture-assertion','expected',%d);
Object.defineProperty(el,'value',{configurable:true,get:function(){throw new Error('owned getter failed');}});
%s return promise;
})`, AssertValueScript, timeoutMS, mutation)
			bounded, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			if err := EvalAssert(bounded, script); !errors.Is(err, ErrAssertionTimeout) {
				t.Fatalf("throwing predicate did not finish safely: %v", err)
			}
			stats := evalJSON(t, ctx, `fixtureObservers`)
			if stats["active"] != float64(0) {
				t.Fatalf("throwing predicate left %v connected observers", stats["active"])
			}
		})
	}
}

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
