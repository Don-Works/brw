package navpolicy

import "testing"

func TestParseEmptyIsNil(t *testing.T) {
	if p := Parse("", ""); !p.Empty() {
		t.Fatalf("empty allow+block should yield an empty policy, got %+v", p)
	}
	if p := Parse("  ,  ", " "); !p.Empty() {
		t.Fatalf("whitespace-only lists should yield an empty policy, got %+v", p)
	}
}

func TestBlocklistMatchesDomainAndSubdomains(t *testing.T) {
	p := Parse("", "evil.com, ads.example.net")
	for _, blocked := range []string{
		"https://evil.com/x",
		"http://www.evil.com",
		"evil.com",
		"https://deep.sub.evil.com/path?q=1",
		"https://ads.example.net",
	} {
		if err := p.Check(blocked); err == nil {
			t.Errorf("expected %q to be blocked", blocked)
		}
	}
	for _, ok := range []string{
		"https://notevil.com",      // suffix must be on a dot boundary
		"https://evilcom.org",      // not a subdomain
		"https://example.net",      // parent of ads.example.net is allowed
		"https://good.example.org", // unrelated
	} {
		if err := p.Check(ok); err != nil {
			t.Errorf("expected %q to be allowed, got %v", ok, err)
		}
	}
}

func TestAllowlistOnlyPermitsListed(t *testing.T) {
	p := Parse("example.com, *.trusted.org", "")
	for _, ok := range []string{
		"https://example.com",
		"https://www.example.com/cart",
		"https://api.trusted.org",
		"trusted.org",
	} {
		if err := p.Check(ok); err != nil {
			t.Errorf("expected %q allowed, got %v", ok, err)
		}
	}
	for _, blocked := range []string{
		"https://evil.com",
		"https://example.com.attacker.net", // not the allowed apex
		"https://nottrusted.org",
	} {
		if err := p.Check(blocked); err == nil {
			t.Errorf("expected %q to be denied by allowlist", blocked)
		}
	}
}

func TestBlockWinsOverAllow(t *testing.T) {
	p := Parse("example.com", "promo.example.com")
	if err := p.Check("https://www.example.com"); err != nil {
		t.Errorf("apex subdomain should be allowed: %v", err)
	}
	if err := p.Check("https://promo.example.com"); err == nil {
		t.Errorf("blocked subdomain must lose to allowlist membership")
	}
}

func TestNonNetworkSchemesAreNotGated(t *testing.T) {
	p := Parse("example.com", "") // allowlist mode — strictest
	for _, u := range []string{"about:blank", "data:text/html,<b>x</b>", "blob:abc", "", "javascript:void(0)"} {
		if err := p.Check(u); err != nil {
			t.Errorf("non-network scheme %q must not be gated, got %v", u, err)
		}
	}
}

func TestSchemeDefaultsToHTTPS(t *testing.T) {
	p := Parse("", "evil.com")
	if err := p.Check("evil.com/path"); err == nil {
		t.Fatalf("a bare host should be treated as a network destination and blocked")
	}
}
