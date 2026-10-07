// Package navpolicy is an opt-in allow/deny guardrail over navigation destinations.
package navpolicy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Policy holds optional allow/deny domain lists.
type Policy struct {
	// Allowed, when non-empty, switches to allowlist mode: ONLY these domains (and their subdomains) may be opened.
	Allowed []string
	// Blocked domains (and their subdomains) may never be opened.
	Blocked []string
}

// NormalizeNavigationURL canonicalizes a browser-navigation target before it is checked or handed to Chrome.
func NormalizeNavigationURL(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "about:blank", nil
	}
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")

	if strings.HasPrefix(raw, "//") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	} else if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, "#") {
		return "", errors.New("relative browser navigation requires an absolute URL or bare host; path/query/fragment-only targets are not accepted")
	} else if !hasScheme(raw) {
		raw = "https://" + raw
	}

	lower := strings.ToLower(raw)
	if lower == "about:blank" || lower == "about:newtab" {
		return lower, nil
	}

	for _, scheme := range []string{"javascript:", "vbscript:"} {
		if strings.HasPrefix(lower, scheme) {
			return "", fmt.Errorf("refusing to navigate to a %s URL: that executes script in the current page rather than navigating. Use brw_evaluate to run JavaScript", strings.TrimSuffix(scheme, ":"))
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid navigation URL %q: %w", clip(rawURL), err)
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() == "" {
		return "", fmt.Errorf("invalid navigation URL %q: http(s) target has no host", clip(rawURL))
	}
	return u.String(), nil
}

// CheckNavigation canonicalizes and checks a browser navigation target.
func (p *Policy) CheckNavigation(rawURL string) (string, error) {
	normalized, err := NormalizeNavigationURL(rawURL)
	if err != nil {
		return "", err
	}
	if err := p.Check(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// Parse builds a Policy from comma/space-separated allow and block lists.
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

		return nil
	}

	if host == "" {

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

func hasScheme(raw string) bool {
	raw = strings.TrimSpace(raw)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == ':':
			return i > 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':

		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return false
}

func isBenignBlank(raw string) bool {
	r := strings.ToLower(strings.TrimSpace(raw))
	return r == "" || r == "about:blank" || r == "about:newtab"
}

func clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func hostOf(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}

	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)

	raw = strings.ReplaceAll(raw, "\\", "/")

	if strings.HasPrefix(raw, "//") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
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

func hostMatches(host, domain string) bool {
	host = asciiHost(host)
	domain = asciiHost(domain)
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func asciiHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return ""
	}
	if ascii, err := idna.Lookup.ToASCII(h); err == nil {
		return ascii
	}
	return h
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

// CheckSubresource gates a SUBRESOURCE request (image, script, font, stylesheet, fetch/XHR, WebSocket, EventSource, beacon) rather than a navigation.
func (p *Policy) CheckSubresource(rawURL string) error {
	if p.Empty() {
		return nil
	}
	host := subresourceHostOf(rawURL)
	if host != "" {
		for _, b := range p.Blocked {
			if hostMatches(host, b) {
				return fmt.Errorf("subresource request to %q is blocked by brw policy (blocked domain %q)", host, b)
			}
		}
	}
	if len(p.Allowed) == 0 {
		return nil
	}
	if host == "" {

		return nil
	}
	for _, a := range p.Allowed {
		if hostMatches(host, a) {
			return nil
		}
	}
	return fmt.Errorf("subresource request to %q is not on the brw allowlist (--allowed-domains)", host)
}

func subresourceHostOf(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "wss://"):
		return hostOf("https://" + trimmed[len("wss://"):])
	case strings.HasPrefix(lower, "ws://"):
		return hostOf("http://" + trimmed[len("ws://"):])
	}
	return hostOf(trimmed)
}

// Confines reports whether the policy restricts destinations at all, i.e.
func (p *Policy) Confines() bool {
	return p != nil && !p.Empty()
}
