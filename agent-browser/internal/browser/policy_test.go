package browser

import (
	"errors"
	"testing"
)

func TestDefaultPolicyAllowsNormalActionAndOpenWeb(t *testing.T) {
	settings := DefaultPolicySettings()
	if !settings.PurchaseGate {
		t.Fatal("default purchase gate should be enforced")
	}
	if settings.PurchaseAuthorized {
		t.Fatal("default purchase authorization should be off")
	}
	if len(settings.Allow) != 0 || len(settings.Deny) != 0 {
		t.Fatalf("default origin lists should be empty (open web): allow=%v deny=%v", settings.Allow, settings.Deny)
	}

	// A normal, non-purchase action on an arbitrary origin is allowed.
	decision := EvaluateActionPolicy(settings, "https://example.com/page", "Add to basket", "")
	if decision.Blocked {
		t.Fatalf("normal action unexpectedly blocked: %+v", decision)
	}
	if decision.Warning != "" {
		t.Fatalf("normal action should carry no warning, got %q", decision.Warning)
	}
}

func TestPurchaseGateBlocksUntilAuthorized(t *testing.T) {
	settings := DefaultPolicySettings()

	cases := []struct{ label, href string }{
		{"Place order", ""},
		{"Pay now", ""},
		{"Complete purchase", ""},
		{"Buy now", ""},
		{"Proceed to checkout", ""},
		{"Continue", "https://shop.example.com/checkout"},
	}
	for _, tc := range cases {
		decision := EvaluateActionPolicy(settings, "https://shop.example.com/cart", tc.label, tc.href)
		if !decision.Blocked {
			t.Fatalf("purchase-like action %q/%q should be blocked by default gate: %+v", tc.label, tc.href, decision)
		}
		if !errors.Is(decision.Error(), ErrBlockedByPolicy) {
			t.Fatalf("blocked decision error should wrap ErrBlockedByPolicy, got %v", decision.Error())
		}
	}

	// Once authorized, the same action is allowed but still surfaces an advisory.
	settings.PurchaseAuthorized = true
	decision := EvaluateActionPolicy(settings, "https://shop.example.com/cart", "Place order", "")
	if decision.Blocked {
		t.Fatalf("authorized purchase action should not be blocked: %+v", decision)
	}
	if decision.Warning == "" {
		t.Fatal("authorized purchase action should still surface an advisory warning")
	}

	// Disabling the gate entirely also allows it (open-web override).
	settings.PurchaseAuthorized = false
	settings.PurchaseGate = false
	decision = EvaluateActionPolicy(settings, "https://shop.example.com/cart", "Place order", "")
	if decision.Blocked {
		t.Fatalf("gate-disabled purchase action should not be blocked: %+v", decision)
	}
}

func TestOriginAllowDenyLists(t *testing.T) {
	// Deny list always blocks, even with the purchase gate satisfied.
	deny := PolicySettings{Deny: []string{"ads.example.com"}}.normalize()
	decision := EvaluateActionPolicy(deny, "https://ads.example.com/x", "Read", "")
	if !decision.Blocked || decision.Category != "origin_denied" {
		t.Fatalf("denied origin should be blocked: %+v", decision)
	}
	// Subdomain of a denied origin is also blocked.
	decision = EvaluateActionPolicy(deny, "https://sub.ads.example.com/x", "Read", "")
	if !decision.Blocked {
		t.Fatalf("subdomain of denied origin should be blocked: %+v", decision)
	}
	// A different origin is unaffected by the deny list.
	decision = EvaluateActionPolicy(deny, "https://example.com/x", "Read", "")
	if decision.Blocked {
		t.Fatalf("non-denied origin should be allowed: %+v", decision)
	}

	// Allow list, when non-empty, blocks anything not listed.
	allow := PolicySettings{Allow: []string{"example.com"}}.normalize()
	decision = EvaluateActionPolicy(allow, "https://www.example.com/x", "Read", "")
	if decision.Blocked {
		t.Fatalf("allow-listed origin (subdomain) should be permitted: %+v", decision)
	}
	decision = EvaluateActionPolicy(allow, "https://other.test/x", "Read", "")
	if !decision.Blocked || decision.Category != "origin_not_allowed" {
		t.Fatalf("origin off the allow list should be blocked: %+v", decision)
	}
}

func TestPolicyStoreGetSetRoundtrip(t *testing.T) {
	store := NewPolicyStore()
	got := store.Get()
	if !got.PurchaseGate {
		t.Fatal("fresh store should default to gate enforced")
	}

	updated := store.Set(PolicySettings{
		PurchaseGate:       true,
		PurchaseAuthorized: true,
		Allow:              []string{"HTTPS://Example.COM/path", "example.com", "shop.test"},
		Deny:              []string{"ads.test:443"},
	})
	if !updated.PurchaseAuthorized {
		t.Fatal("set should persist purchase authorization")
	}
	// Allow list is normalized to bare hosts and de-duplicated.
	if len(updated.Allow) != 2 {
		t.Fatalf("allow list should normalize+dedupe to 2 hosts, got %v", updated.Allow)
	}
	for _, h := range updated.Allow {
		if h != "example.com" && h != "shop.test" {
			t.Fatalf("unexpected normalized allow host %q", h)
		}
	}
	if len(updated.Deny) != 1 || updated.Deny[0] != "ads.test" {
		t.Fatalf("deny host should be normalized to bare host, got %v", updated.Deny)
	}

	// A subsequent Get reflects the stored state.
	again := store.Get()
	if !again.PurchaseAuthorized || len(again.Allow) != 2 {
		t.Fatalf("Get after Set did not reflect stored state: %+v", again)
	}
	// Mutating the returned slice must not corrupt stored state.
	again.Allow[0] = "mutated"
	if store.Get().Allow[0] == "mutated" {
		t.Fatal("Get must return defensive copies of slices")
	}
}

func TestRegistrableDomainAndCrossDomainTransition(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://www.example.com/x", "example.com"},
		{"https://example.com", "example.com"},
		{"https://shop.example.co.uk/cart", "example.co.uk"},
		{"https://a.b.example.com", "example.com"},
		{"localhost", "localhost"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := RegistrableDomain(tc.raw); got != tc.want {
			t.Fatalf("RegistrableDomain(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	if domain, crossed := CrossDomainTransition("https://example.com/a", "https://example.com/b"); crossed {
		t.Fatalf("same-domain navigation should not flag transition, got %q", domain)
	}
	if domain, crossed := CrossDomainTransition("https://www.example.com/a", "https://example.com/b"); crossed {
		t.Fatalf("subdomain-to-apex on same registrable domain should not flag, got %q", domain)
	}
	domain, crossed := CrossDomainTransition("https://example.com/a", "https://payments.test/checkout")
	if !crossed || domain != "payments.test" {
		t.Fatalf("cross-domain navigation should flag new registrable domain, got %q crossed=%v", domain, crossed)
	}
}
