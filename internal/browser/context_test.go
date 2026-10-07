package browser

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTabPinKindsPreserveOwnershipSemantics(t *testing.T) {
	owned := WithCurrentOwnedTabID(context.Background(), "42")
	if got := TabIDFromContext(owned); got != "42" {
		t.Fatalf("owned tab id = %q, want 42", got)
	}
	if TabIDIsExplicit(owned) || !TabIDRequiresCurrentOwnership(owned) {
		t.Fatalf("current-owned pin flags: explicit=%t requires_ownership=%t", TabIDIsExplicit(owned), TabIDRequiresCurrentOwnership(owned))
	}

	// A sequence retarget remains server-owned and therefore reconnect-checked.
	retargeted := WithImplicitTabID(owned, "43")
	if got := TabIDFromContext(retargeted); got != "43" || TabIDIsExplicit(retargeted) || !TabIDRequiresCurrentOwnership(retargeted) {
		t.Fatalf("retargeted pin: id=%q explicit=%t requires_ownership=%t", got, TabIDIsExplicit(retargeted), TabIDRequiresCurrentOwnership(retargeted))
	}

	// A lease is implicit/retargetable but not the bridge's single global pin;
	// it must not inherit the current-ownership gate accidentally.
	leased := WithImplicitTabID(context.Background(), "44")
	if TabIDIsExplicit(leased) || TabIDRequiresCurrentOwnership(leased) {
		t.Fatalf("leased pin flags: explicit=%t requires_ownership=%t", TabIDIsExplicit(leased), TabIDRequiresCurrentOwnership(leased))
	}

	// A caller-supplied tab id always overrides an inherited owned marker.
	explicit := WithTabID(owned, "45")
	if got := TabIDFromContext(explicit); got != "45" || !TabIDIsExplicit(explicit) || TabIDRequiresCurrentOwnership(explicit) {
		t.Fatalf("explicit pin: id=%q explicit=%t requires_ownership=%t", got, TabIDIsExplicit(explicit), TabIDRequiresCurrentOwnership(explicit))
	}
}

func TestSpecializedTabContextsEnforceAccessBeforeBrowserWork(t *testing.T) {
	denied := errors.New("fixture private tab access denied")
	ctx := WithTabID(context.Background(), "private-tab")
	for name, call := range map[string]func(*Manager) error{
		"devtools": func(m *Manager) error {
			_, _, cancel, err := m.devtoolsContext(ctx, time.Second)
			if cancel != nil {
				cancel()
			}
			return err
		},
		"dialog": func(m *Manager) error {
			_, err := m.Dialog(ctx, DialogOptions{Action: "status", TabID: "private-tab"})
			return err
		},
		"stream": func(m *Manager) error { _, err := m.tabContextFor(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			m := &Manager{remote: &RemoteTarget{ExpiresAt: time.Now().Add(-time.Hour)}}
			m.SetTabAccessGuard(func(got context.Context, id string) error {
				if got != ctx || id != "private-tab" {
					t.Fatalf("guard got context %v and tab %q", got, id)
				}
				return denied
			})
			if err := call(m); !errors.Is(err, denied) {
				t.Fatalf("tab operation = %v, want the access refusal before browser work", err)
			}
		})
	}
}
