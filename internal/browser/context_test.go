package browser

import (
	"context"
	"testing"
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
