// Package navpolicy is an opt-in allow/deny guardrail over navigation
// destinations. It is enforced at the agent-facing MCP surface so a
// prompt-injected or confused agent cannot steer the browser to an off-limits
// domain. It is a guardrail, not an anti-fraud or anti-bot control.
package navpolicy

import (
	"fmt"
	"net/url"
	"strings"
)

// Policy holds optional allow/deny domain lists. An empty policy permits
// everything. Blocked always wins over Allowed.
type Policy struct {
	// Allowed, when non-empty, switches to allowlist mode: ONLY these domains
	// (and their subdomains) may be opened.
	Allowed []string
	// Blocked domains (and their subdomains) may never be opened.
	Blocked []string
}

// Parse builds a Policy from comma/space-separated allow and block lists. Entries
// are lowercased and trimmed; a leading "*." or "." is stripped so "*.evil.com",
// ".evil.com", and "evil.com" are equivalent. Returns nil when both are empty.
func Parse(allowed, blocked string) *Policy {
	a := splitDomains(allowed)
	b := splitDomains(blocked)
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	return &Policy{Allowed: a, Blocked: b}
}

// Empty reports whether the policy permits everything (nil or no entries).
func (p *Policy) Empty() bool {
	return p == nil || (len(p.Allowed) == 0 && len(p.Blocked) == 0)
}

// Check returns a descriptive error when rawURL is not permitted. A nil/empty
// policy, and non-network schemes (about:, data:, blob:, javascript:), are always
// allowed — only http(s) destinations are gated. The scheme defaults to https to
// match brw_open, so a bare "example.com/x" is treated as a network destination.
func (p *Policy) Check(rawURL string) error {
	if p.Empty() {
		return nil
	}
	host := hostOf(rawURL)
	if host == "" {
		// Non-network or unparseable (about:blank, data:, blob:) — not gated.
		return nil
	}
	for _, b := range p.Blocked {
		if hostMatches(host, b) {
			return fmt.Errorf("navigation to %q is blocked by brw policy (blocked domain %q); set or adjust --blocked-domains/--allowed-domains to change this", host, b)
		}
	}
	if len(p.Allowed) > 0 {
		for _, a := range p.Allowed {
			if hostMatches(host, a) {
				return nil
			}
		}
		return fmt.Errorf("navigation to %q is not permitted: it is not on the brw allowlist (--allowed-domains)", host)
	}
	return nil
}

func hostOf(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	for _, scheme := range []string{"about:", "data:", "blob:", "javascript:", "chrome:", "file:"} {
		if strings.HasPrefix(lower, scheme) {
			return ""
		}
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostMatches reports whether host equals domain or is a subdomain of it.
func hostMatches(host, domain string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func splitDomains(csv string) []string {
	fields := strings.FieldsFunc(csv, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		d := strings.ToLower(strings.TrimSpace(f))
		d = strings.TrimPrefix(d, "*.")
		d = strings.TrimPrefix(d, ".")
		d = strings.TrimSuffix(d, ".")
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
