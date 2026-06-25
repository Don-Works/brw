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

// Check returns a descriptive error when rawURL is not permitted.
//
// Blocklist-only mode (no Allowed entries) gates only http(s) destinations
// whose host matches a blocked domain; non-network schemes and bare blank pages
// pass. Allowlist mode is a strict confinement boundary and fails CLOSED: ONLY
// http(s) URLs whose host is on the allowlist are permitted. Non-http schemes
// (file:, chrome:, data:, javascript:, blob:) — which can read local files,
// reach browser internals, or execute attacker-controlled content — unparseable
// URLs, and empty hosts are all rejected, so the allowlist cannot be escaped by
// switching schemes or by feeding a URL Go rejects but Chrome accepts. (Bare
// about:blank / about:newtab are allowed in both modes as benign.)
func (p *Policy) Check(rawURL string) error {
	if p.Empty() {
		return nil
	}
	host := hostOf(rawURL)
	if host != "" {
		for _, b := range p.Blocked {
			if hostMatches(host, b) {
				return fmt.Errorf("navigation to %q is blocked by brw policy (blocked domain %q); set or adjust --blocked-domains/--allowed-domains to change this", host, b)
			}
		}
	}
	if len(p.Allowed) == 0 {
		// Blocklist-only mode: anything not explicitly blocked is allowed,
		// including non-network schemes. This is a denylist, not confinement.
		return nil
	}
	// Allowlist mode: fail closed on anything we cannot positively confirm is an
	// allowed http(s) host.
	if host == "" {
		// A bare blank page or a scheme-less RELATIVE reference (e.g. "/api/x",
		// "?q=1") is same-origin — it resolves against the current allowlisted
		// page and cannot escape the allowlist, so it passes. Only a non-http
		// SCHEME (file:, chrome:, data:, javascript:, blob:) or an unparseable
		// absolute URL is blocked.
		if isBenignBlank(rawURL) || !hasScheme(rawURL) {
			return nil
		}
		return fmt.Errorf("navigation to %q is not permitted under the brw allowlist: only http(s) destinations on --allowed-domains are allowed (non-http schemes such as file:, chrome:, data:, javascript:, blob: and unparseable URLs are blocked)", clip(rawURL))
	}
	for _, a := range p.Allowed {
		if hostMatches(host, a) {
			return nil
		}
	}
	return fmt.Errorf("navigation to %q is not permitted: it is not on the brw allowlist (--allowed-domains)", host)
}

// hasScheme reports whether raw begins with a URL scheme ("scheme:"), per
// RFC 3986: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ) ":". A string without one
// is a relative reference (e.g. "/path", "?q", "app/x"), which cannot change
// origin. Used to distinguish a same-origin relative URL from a non-http scheme
// in allowlist mode.
func hasScheme(raw string) bool {
	raw = strings.TrimSpace(raw)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == ':':
			return i > 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			// scheme char anywhere
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false // scheme must start with a letter
			}
		default:
			return false // any other char before ':' => not a scheme
		}
	}
	return false
}

// isBenignBlank reports whether raw is an empty target or a blank page that
// navigates nowhere, so it need not be confined by the allowlist.
func isBenignBlank(raw string) bool {
	r := strings.ToLower(strings.TrimSpace(raw))
	return r == "" || r == "about:blank" || r == "about:newtab"
}

// clip bounds an untrusted URL before it lands in an error string so a giant
// data: URL cannot blow up logs/responses.
func clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// hostOf returns the lowercased host of an http(s) destination, or "" for a
// non-network scheme, a bare blank page, or anything that does not parse to an
// http(s) URL with a host. It normalises the two transformations Chrome applies
// that Go's net/url does not — backslashes act as forward slashes in the
// authority of a special scheme, and tab/CR/LF are stripped — so a host like
// "evil.com\@allowed.com" or "ev\til.com" cannot smuggle a different effective
// host past the policy than the one the browser will navigate to.
func hostOf(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	// Chrome strips ASCII tab/CR/LF from URLs entirely before parsing.
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	// In special-scheme URLs Chrome treats "\" as "/". Normalising here means
	// Go's parser sees the same authority boundary the browser will.
	raw = strings.ReplaceAll(raw, "\\", "/")
	lower := strings.ToLower(raw)
	for _, scheme := range []string{"about:", "data:", "blob:", "javascript:", "chrome:", "chrome-extension:", "file:", "filesystem:", "view-source:", "ftp:", "ws:", "wss:"} {
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
	if u.Scheme != "http" && u.Scheme != "https" {
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
