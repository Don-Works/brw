package browser

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// Low-entropy and obviously fabricated. The assertions below hunt for this
// exact literal, so it must never resemble a real credential.
const fixtureTypedValue = "fixture-login-value-one"

// A credential a recipe resolves is typed into whatever field the recipe
// targets, and internal/snapshot blanks an element's value only for password,
// hidden and cc/one-time-code autocomplete fields. An API token box, or a login
// field a site masks in JavaScript, comes back as an ordinary textbox with its
// value in it — so the post-action state brwd keeps per tab must not contain
// element values at all.
func TestSemanticStateKeepsNoElementValue(t *testing.T) {
	snap := func(value string) snapshot.PageSnapshot {
		return snapshot.PageSnapshot{
			URL:      "https://login.example.test/",
			Title:    "Sign in",
			Metadata: map[string]any{"focused_ref": "e2"},
			Elements: []snapshot.Element{
				{Ref: "e1", Role: "textbox", Name: "Email", Value: "fixture-user-one", Visible: true},
				{Ref: "e2", Role: "textbox", Name: "API token", Value: value, Visible: true},
			},
		}
	}
	for name, test := range map[string]struct {
		state SemanticState
	}{
		"fresh state": {state: NewSemanticState(snap(fixtureTypedValue))},
		"state stored on the tab": {state: func() SemanticState {
			manager := &Manager{lastState: map[string]*SemanticState{}, versions: map[string]int64{}}
			manager.storeState("tab-1", NewSemanticState(snap(fixtureTypedValue)))
			return *manager.lastState["tab-1"]
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			for field, got := range map[string]string{
				"URL":       test.state.URL,
				"Title":     test.state.Title,
				"Focus":     test.state.Focus,
				"Signature": test.state.Signature,
			} {
				if strings.Contains(got, fixtureTypedValue) {
					t.Fatalf("SemanticState.%s retains the typed value: %q", field, got)
				}
			}
			// The signature has to still be a signature, or this proves only
			// that the field was emptied.
			if test.state.Signature == NewSemanticState(snap("fixture-other-value")).Signature {
				t.Fatal("two different typed values produced the same signature; the change detector no longer detects")
			}
		})
	}
}
