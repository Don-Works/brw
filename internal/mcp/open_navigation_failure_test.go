package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// navigationOutcomeController opens a tab whose navigation ended as outcome
// says, the way the extension bridge reports it.
type navigationOutcomeController struct {
	fakeController
	outcome browser.NavigationOutcome
}

func (c navigationOutcomeController) Open(_ context.Context, targetURL string) (browser.OpenResult, error) {
	result := browser.OpenResult{Tab: browser.Tab{ID: "77", URL: targetURL}, Ready: true}
	result.ApplyNavigationOutcome(c.outcome, false)
	return result, nil
}

func TestBrwOpenReportsAFailedNavigationAsAToolError(t *testing.T) {
	tests := []struct {
		name        string
		outcome     browser.NavigationOutcome
		wantError   bool
		wantText    []string
		wantFields  map[string]any
		wantMissing []string
	}{
		{
			name: "http basic auth on the extension bridge",
			outcome: browser.NavigationOutcome{URL: "https://preview.test/", CommittedURL: browser.ErrorPageURL,
				Error: "net::ERR_INVALID_AUTH_CREDENTIALS", HTTPStatus: 401,
				AuthRequired: &browser.AuthChallenge{Scheme: "Basic", Realm: "Preview"}},
			wantError: true,
			wantText:  []string{"navigation failed:", "HTTP 401", `realm "Preview"`, "CDP lane", "tab_id 77", "brw_close_tab"},
			wantFields: map[string]any{
				"error": "navigation_failed", "retryable": false, "ready": false,
				"navigation_error": "net::ERR_INVALID_AUTH_CREDENTIALS", "http_status": float64(401),
			},
		},
		{
			name:        "loaded page",
			outcome:     browser.NavigationOutcome{URL: "https://ok.test/", CommittedURL: "https://ok.test/"},
			wantFields:  map[string]any{"ready": true},
			wantMissing: []string{"navigation_error", "warning", "auth_required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := lineJSON(t, map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "brw_open", "arguments": map[string]any{"url": "https://preview.test/"}},
			})
			var output bytes.Buffer
			if err := New(navigationOutcomeController{outcome: tt.outcome}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			resp := parseLineResponse(t, output.Bytes())
			result, ok := resp["result"].(map[string]any)
			if !ok {
				t.Fatalf("expected result, got %v", resp)
			}
			if isError, _ := result["isError"].(bool); isError != tt.wantError {
				t.Fatalf("isError = %v, want %v (result %v)", result["isError"], tt.wantError, result)
			}
			text := result["content"].([]any)[0].(map[string]any)["text"].(string)
			for _, want := range tt.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("text = %q, missing %q", text, want)
				}
			}
			structured, ok := result["structuredContent"].(map[string]any)
			if !ok {
				t.Fatalf("no structuredContent in %v", result)
			}
			for name, want := range tt.wantFields {
				if structured[name] != want {
					t.Errorf("structuredContent[%q] = %v, want %v", name, structured[name], want)
				}
			}
			for _, name := range tt.wantMissing {
				if _, present := structured[name]; present {
					t.Errorf("structuredContent carries %q on a page that loaded", name)
				}
			}
			if tab, _ := structured["tab"].(map[string]any); tab == nil || tab["id"] != "77" {
				t.Errorf("structuredContent tab = %v, want the opened tab", structured["tab"])
			}
		})
	}
}
