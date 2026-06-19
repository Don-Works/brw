package browser

import (
	"strings"
	"testing"
)

func TestNormalizeNotifyOptionsDefaultsAndValidation(t *testing.T) {
	cases := []struct {
		name      string
		in        NotifyOptions
		wantKind  string
		wantTitle string
		wantMsg   string
		wantErr   bool
	}{
		{
			name:      "empty kind defaults to needs_input with default title",
			in:        NotifyOptions{Message: "  enter code  "},
			wantKind:  "needs_input",
			wantTitle: "brw: action needed",
			wantMsg:   "enter code",
		},
		{
			name:      "done kind keeps explicit title",
			in:        NotifyOptions{Kind: "DONE", Title: "Checkout complete", Message: "Order placed"},
			wantKind:  "done",
			wantTitle: "Checkout complete",
			wantMsg:   "Order placed",
		},
		{
			name:      "error kind default title",
			in:        NotifyOptions{Kind: "error"},
			wantKind:  "error",
			wantTitle: "brw: error",
		},
		{
			name:    "unknown kind is rejected, not silently coerced",
			in:      NotifyOptions{Kind: "celebrate"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeNotifyOptions(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for kind %q, got %#v", tc.in.Kind, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", got.Title, tc.wantTitle)
			}
			if got.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", got.Message, tc.wantMsg)
			}
		})
	}
}

// TestPageNotifyScriptIsWellFormed guards the in-page direct-CDP fallback: it
// must be a single invocable function expression that branches on
// Notification.permission and reports an honest delivery channel instead of
// faking success. This is the plumbing the Manager evaluates over CDP when no
// extension bridge is present.
func TestPageNotifyScriptIsWellFormed(t *testing.T) {
	if !strings.HasPrefix(PageNotifyScript, "(function(opts)") {
		t.Fatalf("PageNotifyScript must be an invocable function expression: %q", PageNotifyScript[:40])
	}
	for _, needle := range []string{
		"Notification.permission",
		"delivery: 'page'",
		"delivery: 'unavailable'",
		"opts.title",
		"opts.message",
	} {
		if !strings.Contains(PageNotifyScript, needle) {
			t.Fatalf("PageNotifyScript missing %q", needle)
		}
	}
	// It must never claim a successful desktop delivery unconditionally — the
	// only ok:true path is gated on a real Notification being constructed.
	if strings.Contains(PageNotifyScript, "ok: true, delivery: 'extension'") {
		t.Fatal("direct-CDP fallback must not claim extension delivery")
	}
}
