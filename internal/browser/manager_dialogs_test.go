package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// openDialogFixtureTab creates a tab on an inline fixture and makes it active,
// which also binds the Manager's tab context (and its dialog listener) to it.
func openDialogFixtureTab(t *testing.T, m *Manager, ctx context.Context, html string) string {
	t.Helper()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html," + html).Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	return string(id)
}

// chromedp enables the Page domain on every target, which suppresses Chrome's
// native dialog UI and blocks the renderer until the dialog is answered over
// CDP. Before the listener existed, this alert() wedged the tab permanently:
// the triggering evaluate AND every later evaluate timed out.
func TestUnansweredDialogDoesNotWedgeTheRenderer(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	openDialogFixtureTab(t, m, ctx, `<html><body><p>fixture</p></body></html>`)

	evalCtx, evalCancel := context.WithTimeout(ctx, 10*time.Second)
	defer evalCancel()
	if _, err := m.Evaluate(evalCtx, `(function(){ alert('wedge me'); return 1; })()`); err != nil {
		t.Fatalf("evaluate that raises alert() should return once the dialog is answered: %v", err)
	}

	// The real regression signal: the tab still responds afterwards.
	afterCtx, afterCancel := context.WithTimeout(ctx, 10*time.Second)
	defer afterCancel()
	value, err := m.Evaluate(afterCtx, `1 + 1`)
	if err != nil {
		t.Fatalf("tab is wedged after the dialog: %v", err)
	}
	if got, ok := value.(float64); !ok || got != 2 {
		t.Fatalf("evaluate after dialog = %v, want 2", value)
	}
}

func TestDialogRecordsAndDefaultAnswers(t *testing.T) {
	tests := []struct {
		name          string
		script        string
		wantType      string
		wantAccepted  bool
		wantDecidedBy string
	}{
		{
			name:          "alert is accepted because OK is its only button",
			script:        `(function(){ alert('hello'); return 1; })()`,
			wantType:      "alert",
			wantAccepted:  true,
			wantDecidedBy: "agent_acting",
		},
		{
			name:          "confirm defaults to the non-destructive answer",
			script:        `(function(){ return confirm('Delete everything?'); })()`,
			wantType:      "confirm",
			wantAccepted:  false,
			wantDecidedBy: "user_safe_default",
		},
		{
			name:          "prompt defaults to the non-destructive answer",
			script:        `(function(){ return prompt('Name?', 'default'); })()`,
			wantType:      "prompt",
			wantAccepted:  false,
			wantDecidedBy: "user_safe_default",
		},
	}

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tabID := openDialogFixtureTab(t, m, ctx, `<html><body>x</body></html>`)
			evalCtx, evalCancel := context.WithTimeout(ctx, 10*time.Second)
			defer evalCancel()
			if _, err := m.Evaluate(evalCtx, tt.script); err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			result, err := m.Dialog(ctx, DialogOptions{Action: "status", TabID: tabID})
			if err != nil {
				t.Fatalf("dialog status: %v", err)
			}
			if result.Count != 1 {
				t.Fatalf("recorded %d dialogs, want 1 (%+v)", result.Count, result.Dialogs)
			}
			got := result.Dialogs[0]
			if got.Type != tt.wantType {
				t.Errorf("type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Accepted != tt.wantAccepted {
				t.Errorf("accepted = %v, want %v", got.Accepted, tt.wantAccepted)
			}
			if got.DecidedBy != tt.wantDecidedBy {
				t.Errorf("decided_by = %q, want %q", got.DecidedBy, tt.wantDecidedBy)
			}
			// status consumes by default, so a second read is empty.
			again, err := m.Dialog(ctx, DialogOptions{Action: "status", TabID: tabID})
			if err != nil {
				t.Fatalf("second dialog status: %v", err)
			}
			if again.Count != 0 {
				t.Errorf("status should consume the ring; second read returned %d", again.Count)
			}
		})
	}
}

// Pre-arming is the feature that lets an agent decide a confirm()/prompt()
// outcome without the renderer ever waiting on a round trip to the daemon.
func TestDialogArmingControlsTheOutcome(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		promptText string
		script     string
		want       string
	}{
		{
			name:     "armed accept makes confirm() return true",
			response: "accept",
			script:   `String(confirm('proceed?'))`,
			want:     "true",
		},
		{
			name:     "armed dismiss makes confirm() return false",
			response: "dismiss",
			script:   `String(confirm('proceed?'))`,
			want:     "false",
		},
		{
			name:       "armed prompt text is delivered to the page",
			response:   "accept",
			promptText: "brw-was-here",
			script:     `String(prompt('name?', 'unused-default'))`,
			want:       "brw-was-here",
		},
	}

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tabID := openDialogFixtureTab(t, m, ctx, `<html><body>x</body></html>`)
			armed, err := m.Dialog(ctx, DialogOptions{
				Action:     "expect",
				Response:   tt.response,
				PromptText: tt.promptText,
				TabID:      tabID,
			})
			if err != nil {
				t.Fatalf("arm: %v", err)
			}
			if armed.Armed == nil || armed.Armed.Remaining != 1 {
				t.Fatalf("arm result = %+v, want one pending answer", armed.Armed)
			}

			evalCtx, evalCancel := context.WithTimeout(ctx, 10*time.Second)
			defer evalCancel()
			value, err := m.Evaluate(evalCtx, tt.script)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got, _ := value.(string); got != tt.want {
				t.Fatalf("page saw %q, want %q", got, tt.want)
			}

			status, err := m.Dialog(ctx, DialogOptions{Action: "status", TabID: tabID})
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if status.Count != 1 || status.Dialogs[0].DecidedBy != "armed" {
				t.Fatalf("expected one armed-decided record, got %+v", status.Dialogs)
			}
			// A single-shot arm is consumed.
			if status.Armed != nil {
				t.Errorf("arm should be consumed, still %+v", status.Armed)
			}
		})
	}
}

func TestDialogArmCountAndClear(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	tabID := openDialogFixtureTab(t, m, ctx, `<html><body>x</body></html>`)

	if _, err := m.Dialog(ctx, DialogOptions{Action: "expect", Response: "accept", Count: 3, TabID: tabID}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	evalCtx, evalCancel := context.WithTimeout(ctx, 15*time.Second)
	defer evalCancel()
	value, err := m.Evaluate(evalCtx, `String(confirm('a')) + ',' + String(confirm('b'))`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got, _ := value.(string); got != "true,true" {
		t.Fatalf("page saw %q, want \"true,true\"", got)
	}
	status, err := m.Dialog(ctx, DialogOptions{Action: "status", Peek: true, TabID: tabID})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Armed == nil || status.Armed.Remaining != 1 {
		t.Fatalf("one arm should remain, got %+v", status.Armed)
	}
	// peek must not consume.
	if status.Count != 2 {
		t.Fatalf("peek returned %d records, want 2", status.Count)
	}
	if peeked, err := m.Dialog(ctx, DialogOptions{Action: "status", Peek: true, TabID: tabID}); err != nil || peeked.Count != 2 {
		t.Fatalf("peek should not consume the ring: count=%d err=%v", peeked.Count, err)
	}

	if _, err := m.Dialog(ctx, DialogOptions{Action: "clear", TabID: tabID}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, err := m.Dialog(ctx, DialogOptions{Action: "status", TabID: tabID})
	if err != nil {
		t.Fatalf("status after clear: %v", err)
	}
	if cleared.Armed != nil {
		t.Fatalf("arm should be cleared, got %+v", cleared.Armed)
	}
}

func TestDialogRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		opts    DialogOptions
		wantErr string
	}{
		{"unknown action", DialogOptions{Action: "yolo"}, "unknown dialog action"},
		{"expect without response", DialogOptions{Action: "expect"}, "requires response"},
		{"expect with bad response", DialogOptions{Action: "expect", Response: "maybe"}, "unknown dialog response"},
	}
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openDialogFixtureTab(t, m, ctx, `<html><body>x</body></html>`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.Dialog(ctx, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

var _ = time.Second
