package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/snapshot"
)

// interactionController is a fakeController that also implements the optional
// interaction capabilities, so the routes can be exercised on both sides of the
// capability check: this type for the supported path, a bare fakeController for
// the named refusal.
type interactionController struct {
	*fakeController
	expression   string
	clipboard    browser.ClipboardOptions
	keyDown      browser.KeyHoldOptions
	keyUp        browser.KeyHoldOptions
	pushState    browser.HistoryStateOptions
	focusedRef   string
	focusSnap    bool
	evaluateCall int
}

func (c *interactionController) Evaluate(_ context.Context, expression string) (any, error) {
	c.expression = expression
	c.evaluateCall++
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
	return browser.KeyHoldResult{ActionResult: browser.ActionResult{OK: true}, Key: opts.Key, Held: []string{}}, nil
}

func (c *interactionController) PushState(_ context.Context, opts browser.HistoryStateOptions) (browser.HistoryStateResult, error) {
	c.pushState = opts
	return browser.HistoryStateResult{OK: true, URL: "https://app.test" + opts.URL}, nil
}

func (c *interactionController) Focus(ctx context.Context, ref string) (browser.ActionResult, error) {
	c.focusedRef = ref
	c.focusSnap = browser.WantSnapshotFromCtx(ctx)
	return browser.ActionResult{OK: true, Message: "focused " + ref, Focus: ref, URL: "https://app.test/"}, nil
}

func newInteractionServer() (*Server, *interactionController) {
	ctrl := &interactionController{fakeController: &fakeController{}}
	return New("", ctrl), ctrl
}

func call(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)
	return rec
}

func TestKeyHoldRoutesForwardToTheController(t *testing.T) {
	server, ctrl := newInteractionServer()

	rec := call(t, server, http.MethodPost, "/api/page/key_down", `{"key":"Shift","tab_id":"tab9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("key_down status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.keyDown.Key != "Shift" || ctrl.keyDown.TabID != "tab9" {
		t.Fatalf("key_down forwarded %+v", ctrl.keyDown)
	}
	var down browser.KeyHoldResult
	if err := json.NewDecoder(rec.Body).Decode(&down); err != nil {
		t.Fatalf("decode key_down response: %v", err)
	}
	if len(down.Held) != 1 || down.Held[0] != "Shift" {
		t.Fatalf("key_down response held = %v", down.Held)
	}

	rec = call(t, server, http.MethodPost, "/api/page/key_up", `{"key":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("key_up status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.keyUp.Key != "all" {
		t.Fatalf("key_up forwarded %+v", ctrl.keyUp)
	}
}

func TestInteractionRoutesNameTheMissingCapability(t *testing.T) {
	// A bare fakeController implements browser.Controller and none of the
	// optional interaction capabilities — the extension-bridge shape.
	server := New("", &fakeController{})
	for _, tc := range []struct {
		path string
		body string
		want string
	}{
		{path: "/api/page/key_down", body: `{"key":"Shift"}`, want: "held keys"},
		{path: "/api/page/key_up", body: `{"key":"Shift"}`, want: "held keys"},
		{path: "/api/page/clipboard", body: `{"action":"read"}`, want: "clipboard"},
		{path: "/api/page/pushstate", body: `{"url":"/x"}`, want: "history"},
		{path: "/api/page/focus", body: `{"ref":"e1"}`, want: "focus"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := call(t, server, http.MethodPost, tc.path, tc.body)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s should refuse when the transport lacks the capability; got 200 %s", tc.path, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%s refusal %q should name the capability (%q)", tc.path, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestClipboardRouteForwardsTheAction(t *testing.T) {
	server, ctrl := newInteractionServer()
	rec := call(t, server, http.MethodPost, "/api/page/clipboard", `{"action":"write","text":"copy me"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.clipboard.Action != "write" || ctrl.clipboard.Text != "copy me" {
		t.Fatalf("clipboard forwarded %+v", ctrl.clipboard)
	}
}

func TestPushStateRouteAppliesNavigationPolicy(t *testing.T) {
	server, ctrl := newInteractionServer()
	server.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{"app.test"}})

	rec := call(t, server, http.MethodPost, "/api/page/pushstate", `{"url":"https://evil.example/x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-allowlist pushstate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.pushState.URL != "" {
		t.Fatalf("an off-allowlist pushstate still reached the controller: %+v", ctrl.pushState)
	}

	// A relative route is same-origin by construction and must still pass.
	rec = call(t, server, http.MethodPost, "/api/page/pushstate", `{"url":"/settings","replace":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("relative pushstate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.pushState.URL != "/settings" || !ctrl.pushState.Replace {
		t.Fatalf("pushstate forwarded %+v", ctrl.pushState)
	}
}

func TestFocusRouteForwardsTheRef(t *testing.T) {
	server, ctrl := newInteractionServer()
	rec := call(t, server, http.MethodPost, "/api/page/focus", `{"ref":"e12"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.focusedRef != "e12" {
		t.Fatalf("focused ref = %q", ctrl.focusedRef)
	}
	// The route answers with the post-action observation, not {ok,ref}: an agent
	// has to be able to read focus/url out of the result the way it can for press.
	var observed browser.ActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &observed); err != nil {
		t.Fatalf("decode focus result %s: %v", rec.Body.String(), err)
	}
	if observed.Focus != "e12" || observed.URL == "" {
		t.Fatalf("focus result = %+v, want the observation with focus e12 and a url", observed)
	}
	if ctrl.focusSnap {
		t.Fatal("focus without snapshot:true asked for a snapshot")
	}
	// snapshot:true must reach the controller, the way it does on press/select.
	if rec := call(t, server, http.MethodPost, "/api/page/focus", `{"ref":"e12","snapshot":true}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !ctrl.focusSnap {
		t.Fatal("focus with snapshot:true did not request the snapshot re-materialization")
	}
	if rec := call(t, server, http.MethodPost, "/api/page/focus", `{}`); rec.Code == http.StatusOK {
		t.Fatal("focus with no ref should be rejected")
	}
}

func TestGetRouteAcceptsQueryAndBody(t *testing.T) {
	server, ctrl := newInteractionServer()

	rec := call(t, server, http.MethodGet, "/api/page/get?what=attr&target=%23name&name=data-kind", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := snapshot.GetRequest{What: "attr", Target: "#name", Name: "data-kind"}.Expression()
	if ctrl.expression != want {
		t.Fatalf("GET /api/page/get evaluated\n%s\nwant\n%s", ctrl.expression, want)
	}

	rec = call(t, server, http.MethodPost, "/api/page/get", `{"what":"count","target":".row"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.expression != (snapshot.GetRequest{What: "count", Target: ".row"}).Expression() {
		t.Fatalf("POST /api/page/get evaluated the wrong expression")
	}

	// Validation happens before the browser is touched.
	calls := ctrl.evaluateCall
	rec = call(t, server, http.MethodPost, "/api/page/get", `{"what":"count"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("count with no target should be rejected; got %s", rec.Body.String())
	}
	if ctrl.evaluateCall != calls {
		t.Fatal("an invalid get still reached the browser")
	}
	if !strings.Contains(rec.Body.String(), "requires target") {
		t.Fatalf("refusal %q should say what is missing", rec.Body.String())
	}
}

func TestFrameRouteSwitchesScope(t *testing.T) {
	server, ctrl := newInteractionServer()
	rec := call(t, server, http.MethodPost, "/api/page/frame", `{"target":"#embed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.expression != snapshot.BuildFrameSwitchExpression("#embed") {
		t.Fatalf("frame route evaluated the wrong expression")
	}
	if rec := call(t, server, http.MethodPost, "/api/page/frame", `{"target":"main"}`); rec.Code != http.StatusOK {
		t.Fatalf("frame main status = %d", rec.Code)
	}
	if ctrl.expression != snapshot.BuildFrameSwitchExpression("main") {
		t.Fatal("frame main did not reach the browser")
	}
}
