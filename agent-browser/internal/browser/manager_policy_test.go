package browser

import (
	"context"
	"errors"
	"testing"
)

// newPolicyTestManager builds a Manager with only the policy machinery wired,
// which is all the gate path (guardAction) and policy get/set exercise. It does
// NOT launch Chrome, matching the codebase's pure-logic test idiom.
func newPolicyTestManager() *Manager {
	return &Manager{}
}

func TestManagerGuardActionBlocksPurchaseUntilAuthorized(t *testing.T) {
	m := newPolicyTestManager()

	// Default envelope: purchase gate enforced. A place-order click is refused
	// with an explicit policy error (this is exactly what Manager.Click calls).
	if _, err := m.guardAction("https://shop.example.com/cart", "Place order", ""); err == nil {
		t.Fatal("place-order action should be blocked by the default purchase gate")
	} else if !errors.Is(err, ErrBlockedByPolicy) {
		t.Fatalf("block error should wrap ErrBlockedByPolicy, got %v", err)
	}

	// A normal action is allowed and carries no warning.
	if warning, err := m.guardAction("https://shop.example.com/cart", "Add to basket", ""); err != nil {
		t.Fatalf("normal action should be allowed, got %v", err)
	} else if warning != "" {
		t.Fatalf("normal action should carry no warning, got %q", warning)
	}

	// Authorize purchases via SetPolicy; the same action now proceeds with an
	// advisory warning instead of a block.
	if _, err := m.SetPolicy(context.Background(), PolicySettings{PurchaseGate: true, PurchaseAuthorized: true}); err != nil {
		t.Fatal(err)
	}
	warning, err := m.guardAction("https://shop.example.com/cart", "Place order", "")
	if err != nil {
		t.Fatalf("authorized purchase action should not be blocked, got %v", err)
	}
	if warning == "" {
		t.Fatal("authorized purchase action should still surface an advisory warning")
	}
}

func TestManagerGuardActionRespectsOriginDenyList(t *testing.T) {
	m := newPolicyTestManager()
	if _, err := m.SetPolicy(context.Background(), PolicySettings{PurchaseGate: true, Deny: []string{"blocked.test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.guardAction("https://blocked.test/page", "Read", ""); err == nil {
		t.Fatal("action on denied origin should be blocked")
	}
	if _, err := m.guardAction("https://allowed.test/page", "Read", ""); err != nil {
		t.Fatalf("action on a non-denied origin should be allowed, got %v", err)
	}
}

func TestManagerGetSetPolicyRoundtrip(t *testing.T) {
	m := newPolicyTestManager()
	got, err := m.GetPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.PurchaseGate {
		t.Fatal("fresh manager should default to gate enforced")
	}

	if _, err := m.SetPolicy(context.Background(), PolicySettings{PurchaseGate: false, Allow: []string{"only.test"}}); err != nil {
		t.Fatal(err)
	}
	got, err = m.GetPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PurchaseGate {
		t.Fatal("gate should be disabled after set")
	}
	if len(got.Allow) != 1 || got.Allow[0] != "only.test" {
		t.Fatalf("allow list not persisted: %#v", got.Allow)
	}
}

func TestAnnotateTransitionFlagsCrossDomain(t *testing.T) {
	result := &ActionResult{URL: "https://payments.test/checkout"}
	annotateTransition(result, "https://shop.example.com/cart")
	if result.DomainTransition != "payments.test" {
		t.Fatalf("expected cross-domain transition flag, got %q", result.DomainTransition)
	}

	same := &ActionResult{URL: "https://shop.example.com/checkout"}
	annotateTransition(same, "https://shop.example.com/cart")
	if same.DomainTransition != "" {
		t.Fatalf("same-domain navigation should not flag transition, got %q", same.DomainTransition)
	}
}
