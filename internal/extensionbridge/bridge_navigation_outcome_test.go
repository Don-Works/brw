package extensionbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// previewOutcome is what the extension records for a Vercel preview deployment:
// the paused main document answered 401 with a Basic challenge, and Chrome,
// which cancels the auth prompt under a debugger, then reported the error.
func previewOutcome() map[string]any {
	return map[string]any{
		"known": true, "url": "https://preview.test/", "status": 401,
		"authenticate": []string{`Basic realm="Preview"`},
		"error":        "net::ERR_INVALID_AUTH_CREDENTIALS",
	}
}

func newOpenOutcomeFake(frameURL string, outcome map[string]any) *groupAwareExtension {
	return &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     300,
		groups:        map[int]*gaGroup{},
		tabs:          []*gaTab{{id: 100, windowID: 1, groupID: -1, active: true, url: "https://user.test/", title: "user"}},
		frameURL:      frameURL,
		navOutcome:    outcome,
	}
}

func TestBridgeOpenReportsHowTheNavigationEnded(t *testing.T) {
	tests := []struct {
		name      string
		frameURL  string
		outcome   map[string]any
		wantReady bool
		wantError string
		wantHTTP  int
		wantAuth  *browser.AuthChallenge
		wantHint  []string
	}{
		{
			name: "basic auth challenge", frameURL: browser.ErrorPageURL, outcome: previewOutcome(),
			wantError: "net::ERR_INVALID_AUTH_CREDENTIALS", wantHTTP: 401,
			wantAuth: &browser.AuthChallenge{Scheme: "Basic", Realm: "Preview"},
			wantHint: []string{"answered HTTP 401", `realm "Preview"`, "brw_authenticate is not available on the extension bridge", "CDP lane"},
		},
		{
			name: "dns failure", frameURL: browser.ErrorPageURL,
			outcome:   map[string]any{"known": true, "url": "https://preview.test/", "error": "net::ERR_NAME_NOT_RESOLVED"},
			wantError: "net::ERR_NAME_NOT_RESOLVED",
			wantHint:  []string{"did not load (net::ERR_NAME_NOT_RESOLVED)", "chrome-error://chromewebdata/"},
		},
		{
			name: "extension without navigation_outcome", frameURL: browser.ErrorPageURL,
			wantError: "chrome-error page (the browser reported no error code)",
			wantHint:  []string{"chrome-error://chromewebdata/"},
		},
		{
			name: "loaded page", frameURL: "https://preview.test/",
			outcome:   map[string]any{"known": true, "url": "https://preview.test/", "status": 200},
			wantReady: true,
		},
		{
			name: "rendered 404", frameURL: "https://preview.test/",
			outcome:   map[string]any{"known": true, "url": "https://preview.test/", "status": 404},
			wantReady: true, wantHTTP: 404,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			cleanup := connectGroupAwareExtension(t, b, newOpenOutcomeFake(tt.frameURL, tt.outcome))
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := b.Open(ctx, "https://preview.test/")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if result.Tab.ID == "" {
				t.Fatal("a failed navigation must still report the tab it opened")
			}
			if result.Ready != tt.wantReady || result.NavigationError != tt.wantError || result.HTTPStatus != tt.wantHTTP {
				t.Fatalf("result = %+v, want ready=%t navigation_error=%q http_status=%d", result, tt.wantReady, tt.wantError, tt.wantHTTP)
			}
			if (result.AuthRequired == nil) != (tt.wantAuth == nil) || (tt.wantAuth != nil && *result.AuthRequired != *tt.wantAuth) {
				t.Fatalf("auth_required = %+v, want %+v", result.AuthRequired, tt.wantAuth)
			}
			if len(tt.wantHint) == 0 && result.Warning != "" {
				t.Fatalf("warning = %q on a navigation that did not fail", result.Warning)
			}
			for _, hint := range tt.wantHint {
				if !strings.Contains(result.Warning, hint) {
					t.Errorf("warning = %q, missing %q", result.Warning, hint)
				}
			}
		})
	}
}

func TestBridgeBatchOpenStepStopsOnAFailedNavigation(t *testing.T) {
	for _, runner := range []string{"batch", "plan"} {
		t.Run(runner, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			cleanup := connectGroupAwareExtension(t, b, newOpenOutcomeFake(browser.ErrorPageURL, previewOutcome()))
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stepOK bool
			var stepErr string
			if runner == "batch" {
				res, err := b.ExecuteBatch(ctx, []browser.BatchStep{
					{Action: "open", URL: "https://preview.test/"},
					{Action: "wait", Condition: "text:dashboard", TimeoutMS: 100},
				})
				if err != nil {
					t.Fatalf("ExecuteBatch: %v", err)
				}
				if res.OK || len(res.Steps) == 0 {
					t.Fatalf("batch = %+v, want a failed open step", res)
				}
				stepOK, stepErr = res.Steps[0].OK, res.Steps[0].Error
			} else {
				res, err := b.ExecutePlan(ctx, []browser.PlanStep{
					{Action: "open", URL: "https://preview.test/"},
					{Action: "wait", Condition: "text:dashboard", TimeoutMS: 100},
				})
				if err != nil {
					t.Fatalf("ExecutePlan: %v", err)
				}
				if res.OK || len(res.Steps) == 0 {
					t.Fatalf("plan = %+v, want a failed open step", res)
				}
				stepOK, stepErr = res.Steps[0].OK, res.Steps[0].Error
			}
			if stepOK || !browser.IsNavigationFailedMessage(stepErr) || !strings.Contains(stepErr, "CDP lane") ||
				!strings.Contains(stepErr, "tab_id 300") {
				t.Fatalf("open step ok=%t error=%q, want the navigation failure naming the CDP lane and the tab to close", stepOK, stepErr)
			}
		})
	}
}

func TestBridgeNavigateToReportsHowTheNavigationFailed(t *testing.T) {
	tests := []struct {
		name string
		// errorText is what Page.navigate answers; empty means Chrome committed
		// its error page as an ordinary replacement document instead.
		errorText string
		outcome   map[string]any
		want      []string
		notWant   []string
	}{
		{
			name: "basic auth challenge", errorText: "net::ERR_INVALID_AUTH_CREDENTIALS", outcome: previewOutcome(),
			want: []string{"navigate_to: navigation failed:", "answered HTTP 401", `HTTP Basic authentication (realm "Preview")`, "CDP lane"},
		},
		{
			name: "auth error from an extension without navigation_outcome", errorText: "net::ERR_INVALID_AUTH_CREDENTIALS",
			want: []string{"(net::ERR_INVALID_AUTH_CREDENTIALS)", "asks for HTTP authentication", "CDP lane"},
		},
		{
			name:    "error page committed without an errorText",
			outcome: map[string]any{"known": true, "url": "https://target.test/landing", "error": "net::ERR_NAME_NOT_RESOLVED"},
			want:    []string{"did not load (net::ERR_NAME_NOT_RESOLVED)", "chrome-error://chromewebdata/"},
			notWant: []string{"authentication"},
		},
	}
	for _, tt := range tests {
		for _, runner := range []string{"standalone", "batch"} {
			t.Run(tt.name+"/"+runner, func(t *testing.T) {
				finalURL, finalOrigin := "https://target.test/landing", "https://target.test"
				if tt.errorText == "" {
					finalURL, finalOrigin = browser.ErrorPageURL, "null"
				}
				b, fake, cleanup := newNavigationFake(t, finalURL, finalOrigin, 0, false)
				defer cleanup()
				fake.mu.Lock()
				fake.navigateErrorText = tt.errorText
				fake.navOutcome = tt.outcome
				fake.mu.Unlock()
				ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 3*time.Second)
				defer cancel()
				var message string
				if runner == "standalone" {
					_, err := b.NavigateTo(ctx, "https://target.test/landing")
					var failed *browser.NavigationFailedError
					if !errors.As(err, &failed) {
						t.Fatalf("NavigateTo error = %v, want a NavigationFailedError", err)
					}
					message = err.Error()
				} else {
					res, err := b.ExecuteBatch(ctx, []browser.BatchStep{{Action: "navigate_to", URL: "https://target.test/landing"}})
					if err != nil {
						t.Fatalf("ExecuteBatch: %v", err)
					}
					if res.OK || len(res.Steps) == 0 || res.Steps[0].OK {
						t.Fatalf("batch = %+v, want a failed navigate_to step", res)
					}
					message = res.Steps[0].Error
				}
				for _, want := range tt.want {
					if !strings.Contains(message, want) {
						t.Errorf("error = %q, missing %q", message, want)
					}
				}
				for _, notWant := range tt.notWant {
					if strings.Contains(message, notWant) {
						t.Errorf("error = %q, must not contain %q", message, notWant)
					}
				}
			})
		}
	}
}
