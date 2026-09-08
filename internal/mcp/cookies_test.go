package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// cookieRecordingController captures brw_cookies arguments so the handler test
// can assert the full argument surface reaches the controller unchanged.
type cookieRecordingController struct {
	fakeController
	cookiesCalls []browser.CookieParams
	cookiesErr   error
}

func (c *cookieRecordingController) Cookies(_ context.Context, params browser.CookieParams) (browser.CookieResult, error) {
	c.cookiesCalls = append(c.cookiesCalls, params)
	if c.cookiesErr != nil {
		return browser.CookieResult{}, c.cookiesErr
	}
	return browser.CookieResult{
		Action:  params.Action,
		URL:     "https://example.test/",
		Count:   2,
		Cookies: []browser.Cookie{{Name: "sid", Value: "v", Domain: "example.test", Path: "/", HTTPOnly: true, Secure: true, Session: true}},
	}, nil
}

func callCookiesTool(t *testing.T, ctrl browser.Controller, args string) map[string]any {
	t.Helper()
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_cookies","arguments":` + args + `}}` + "\n"
	var output bytes.Buffer
	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, output.String())
	}
	if resp.Result == nil {
		t.Fatalf("no result object: %s", output.String())
	}
	return resp.Result
}

func TestCookiesToolForwardsAllActions(t *testing.T) {
	ctrl := &cookieRecordingController{}
	cases := []struct {
		args string
		want browser.CookieParams
	}{
		{`{"action":"list"}`, browser.CookieParams{Action: "list"}},
		{`{"action":"set","name":"sid","value":"v","url":"https://example.test/","path":"/","secure":true,"http_only":true,"same_site":"strict","expires":1893456000}`, browser.CookieParams{
			Action: "set", Name: "sid", Value: "v", URL: "https://example.test/", Path: "/",
			Secure: true, HTTPOnly: true, SameSite: "strict", Expires: 1893456000,
		}},
		{`{"action":"delete","name":"sid","domain":"example.test"}`, browser.CookieParams{Action: "delete", Name: "sid", Domain: "example.test"}},
	}
	for i, tc := range cases {
		result := callCookiesTool(t, ctrl, tc.args)
		if _, isErr := result["isError"]; isErr {
			t.Fatalf("case %d: unexpected tool error: %v", i, result)
		}
	}
	if len(ctrl.cookiesCalls) != len(cases) {
		t.Fatalf("controller saw %d cookie calls, want %d", len(ctrl.cookiesCalls), len(cases))
	}
	for i, tc := range cases {
		got := ctrl.cookiesCalls[i]
		if got != tc.want {
			t.Fatalf("case %d params = %+v, want %+v", i, got, tc.want)
		}
	}
}

// The list result must surface the cookie metadata agents act on — HttpOnly
// above all, the whole reason the tool exists.
func TestCookiesToolListResultCarriesCookieMetadata(t *testing.T) {
	result := callCookiesTool(t, &cookieRecordingController{}, `{"action":"list"}`)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in result: %v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload browser.CookieResult
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("result text is not a CookieResult: %v\n%s", err, text)
	}
	if payload.Count != 2 || len(payload.Cookies) != 1 {
		t.Fatalf("unexpected list payload: %+v", payload)
	}
	cookie := payload.Cookies[0]
	if cookie.Name != "sid" || !cookie.HTTPOnly || !cookie.Secure || !cookie.Session {
		t.Fatalf("cookie metadata lost in transit: %+v", cookie)
	}
}

// A transport-limitation error (extension bridge) must surface as a tool error
// carrying the reason, not as an RPC failure — mirroring brw_open_incognito.
func TestCookiesToolSurfacesTransportLimitationAsToolError(t *testing.T) {
	ctrl := &cookieRecordingController{cookiesErr: errors.New("cookie access is not supported on the extension-bridge transport; use a direct-CDP profile")}
	result := callCookiesTool(t, ctrl, `{"action":"list"}`)
	if result["isError"] != true {
		t.Fatalf("expected isError result, got %v", result)
	}
	content, _ := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "extension-bridge") {
		t.Fatalf("error text lost the transport reason: %q", text)
	}
}

func TestCookiesToolIsInCatalogue(t *testing.T) {
	props := toolProperties(t, "brw_cookies")
	for _, param := range []string{"action", "url", "domain", "path", "name", "value", "secure", "http_only", "same_site", "expires"} {
		if _, ok := props[param]; !ok {
			t.Errorf("brw_cookies schema does not advertise %q", param)
		}
	}
	schema := toolByName(t, "brw_cookies")["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("brw_cookies required = %v, want [action]", required)
	}
}

// Progressive disclosure must be able to find the tool by plain intent.
func TestCookiesToolDiscoverableViaSearch(t *testing.T) {
	for _, query := range []string{"list the cookies", "set a cookie", "delete cookie", "httponly cookies"} {
		if !searchRanksFirst(query, "brw_cookies") {
			t.Errorf("query %q does not surface brw_cookies first", query)
		}
	}
}

func searchRanksFirst(query, name string) bool {
	for _, match := range searchTools(query) {
		if match.Name == name {
			return true
		}
	}
	return false
}
