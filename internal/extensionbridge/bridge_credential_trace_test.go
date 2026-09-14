package extensionbridge

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// A guarantee in a tool description has to hold on every transport. The
// direct-CDP manager has its own live-Chrome test for credential redaction;
// this is the same guarantee on the extension bridge, whose trace is read over
// the same HTTP control plane.
//
// The probe value is named as probe data rather than as a credential: the OSS
// hygiene gate flags a literal assigned to something called "secret", and a
// gate people learn to override is worse than a duller name.
const bridgeProbeValue = "fixture-login-value-one"

func TestBridgeTraceWithholdsACredentialSourcedValue(t *testing.T) {
	for name, test := range map[string]struct {
		mark                  func(context.Context) context.Context
		wantRedacted          bool
		wantCredentialSourced bool
	}{
		"credential sourced": {browser.WithCredentialSourced, true, true},
		"caller declared secret": {
			browser.WithSensitiveAction, true, false},
		"unmarked": {func(ctx context.Context) context.Context { return ctx }, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			bridge, _, cleanup := connectPropertyWriteFake(t)
			defer cleanup()
			ctx, cancel := context.WithTimeout(test.mark(browser.WithTabID(context.Background(), "7")), 5*time.Second)
			defer cancel()

			if _, err := bridge.Fill(ctx, snapshot.FillOptions{Ref: "e1", Text: bridgeProbeValue, Replace: true}); err != nil {
				t.Fatalf("fill: %v", err)
			}

			var fills int
			for _, entry := range bridge.GetTrace().Entries {
				if entry.Action != "fill" {
					continue
				}
				fills++
				if entry.Redacted != test.wantRedacted {
					t.Fatalf("trace entry %+v redacted=%v, want %v", entry, entry.Redacted, test.wantRedacted)
				}
				if entry.CredentialSourced != test.wantCredentialSourced {
					t.Fatalf("trace entry %+v credential_sourced=%v, want %v", entry, entry.CredentialSourced, test.wantCredentialSourced)
				}
				if test.wantRedacted && entry.Text != "" {
					t.Fatalf("a redacted bridge trace entry still carries %q", entry.Text)
				}
				if !test.wantRedacted && entry.Text != bridgeProbeValue {
					t.Fatalf("an unmarked fill recorded %q; the marked cases prove nothing if nothing is ever recorded", entry.Text)
				}
			}
			if fills != 1 {
				t.Fatalf("the bridge recorded %d fill entries, want 1", fills)
			}
		})
	}
}
