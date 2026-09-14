package siteconsent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalPrompterAnswers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "y", input: "y\n", want: true},
		{name: "yes", input: "yes\n", want: true},
		{name: "YES with spaces", input: "  YES  \n", want: true},
		{name: "n", input: "n\n", want: false},
		{name: "bare newline is not consent", input: "\n", want: false},
		{name: "anything else is not consent", input: "maybe\n", want: false},
		{name: "yeah is not yes", input: "yeah\n", want: false},
		{name: "closed input is not consent", input: "", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			prompter := NewTerminalPrompter(strings.NewReader(c.input), &out)
			got, err := prompter.AskSite("https://example.test", ScopeAct)
			if err != nil {
				t.Fatalf("AskSite: %v", err)
			}
			if got != c.want {
				t.Fatalf("AskSite(%q) = %v, want %v", c.input, got, c.want)
			}
			if !strings.Contains(out.String(), "https://example.test") {
				t.Fatalf("the prompt does not name the origin: %q", out.String())
			}
		})
	}
}

func TestTerminalPrompterConfirmNamesTheRisk(t *testing.T) {
	var out strings.Builder
	prompter := NewTerminalPrompter(strings.NewReader("y\n"), &out)
	allowed, err := prompter.ConfirmAction(
		ActionRequest{Tool: "brw_click", Origin: "https://shop.example.test", Label: "Place order"},
		[]Risk{{Class: RiskPurchase, Evidence: "place order"}},
	)
	if err != nil || !allowed {
		t.Fatalf("ConfirmAction = %v, %v", allowed, err)
	}
	text := out.String()
	for _, want := range []string{"brw_click", "https://shop.example.test", "Place order", "purchase"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the confirmation prompt omits %q: %q", want, text)
		}
	}
}

// TestTerminalPrompterDrivesTheGuard proves the shipped prompter satisfies the
// interface the guard actually calls, rather than only matching it at compile
// time.
func TestTerminalPrompterDrivesTheGuard(t *testing.T) {
	store, err := NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := NewGuard(store, AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	var out strings.Builder
	// One answer only: the second Authorize must be answered from the record.
	guard.SetPrompter(NewTerminalPrompter(strings.NewReader("y\n"), &out))

	if err := guard.Authorize("https://asked.test/page", ScopeRead); err != nil {
		t.Fatalf("a prompted yes did not authorise: %v", err)
	}
	if err := guard.Authorize("https://asked.test/other", ScopeRead); err != nil {
		t.Fatalf("the recorded answer was not reused: %v", err)
	}
	if strings.Count(out.String(), "Allow?") != 1 {
		t.Fatalf("the user was asked more than once: %q", out.String())
	}
}
