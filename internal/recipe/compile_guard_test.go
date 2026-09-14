package recipe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// A credential-sourced action is a compile FAILURE. The draft generator already
// turns a merely redacted fill into a ${input:TODO…} placeholder, which is the
// right answer when a human supplied the value — they can supply it again. It
// is the wrong answer when a provider supplied it: brw does not record which
// reference produced the value, so a placeholder would be an invitation to
// guess, and a wrong guess types the staging password into production.
func TestTraceToRecipeCompilationRefusesACredentialSourcedAction(t *testing.T) {
	for name, test := range map[string]struct {
		actions []TraceAction
		wantErr bool
	}{
		"credential fill": {[]TraceAction{
			{Action: "fill", Role: "textbox", Name: "Password", CredentialSourced: true, Redacted: true, OK: true},
		}, true},
		"credential type": {[]TraceAction{
			{Action: "click", Role: "button", Name: "Sign in", OK: true},
			{Action: "type", Role: "textbox", Name: "Password", CredentialSourced: true, Redacted: true, OK: true},
		}, true},
		"credential fill that failed": {[]TraceAction{
			{Action: "fill", Role: "textbox", Name: "Password", CredentialSourced: true, Redacted: true, OK: false},
			{Action: "click", Role: "button", Name: "Sign in", OK: true},
		}, true},
		"merely redacted stays a placeholder": {[]TraceAction{
			{Action: "fill", Role: "textbox", Name: "Password", Redacted: true, OK: true},
		}, false},
		"ordinary fill": {[]TraceAction{
			{Action: "fill", Role: "textbox", Name: "Month", Text: "2026-09", OK: true},
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			drafts, err := DraftFromTrace(test.actions, DraftOptions{
				ID: "example.login.sign-in", Origins: []string{"https://login.example.test"},
			})
			if !test.wantErr {
				if err != nil {
					t.Fatalf("DraftFromTrace refused a draftable trace: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("DraftFromTrace produced %d drafts from a credential-sourced trace", len(drafts))
			}
			if !errors.Is(err, ErrCredentialSourcedAction) {
				t.Fatalf("DraftFromTrace error = %v, want ErrCredentialSourcedAction", err)
			}
			if !strings.Contains(err.Error(), "secret://") {
				t.Fatalf("compile failure %q does not tell the human what to write instead", err)
			}
		})
	}
}

// The hook reads the trace shape a compiler actually receives: browser writes
// credential_sourced, and the draft side decodes it under the same name. A
// rename on either side is a silently unguarded compiler, so the round trip is
// the assertion.
func TestTraceEntryCredentialMarkDecodesIntoTheCompilationGuard(t *testing.T) {
	encoded, err := json.Marshal(browser.RedactTraceEntry(
		browser.WithCredentialSourced(t.Context()),
		browser.TraceEntry{Action: "fill", Ref: "e2", Text: "fixture-login-value-one", OK: true},
	))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-login-value-one") {
		t.Fatalf("a credential-sourced trace entry carries the value: %s", encoded)
	}
	var action TraceAction
	if err := json.Unmarshal(encoded, &action); err != nil {
		t.Fatal(err)
	}
	if !action.CredentialSourced {
		t.Fatalf("browser trace %s did not decode as credential-sourced", encoded)
	}
	if err := GuardTraceActionForCompilation(0, action); !errors.Is(err, ErrCredentialSourcedAction) {
		t.Fatalf("guard on a credential-sourced action = %v", err)
	}
}

// Nothing marks an ordinary action, so an ordinary trace still compiles.
func TestAnUnmarkedTraceEntryPassesTheCompilationGuard(t *testing.T) {
	encoded, err := json.Marshal(browser.RedactTraceEntry(t.Context(),
		browser.TraceEntry{Action: "fill", Ref: "e1", Text: "fixture-user-one", OK: true}))
	if err != nil {
		t.Fatal(err)
	}
	var action TraceAction
	if err := json.Unmarshal(encoded, &action); err != nil {
		t.Fatal(err)
	}
	if action.CredentialSourced {
		t.Fatalf("an unmarked action decoded as credential-sourced: %s", encoded)
	}
	if err := GuardTraceActionForCompilation(0, action); err != nil {
		t.Fatalf("guard refused an ordinary action: %v", err)
	}
}
