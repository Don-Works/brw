// Package siteconsent records what a user actually allowed brw to do on a site,
// so there is something to show them and something to revoke.
//
// Domain containment (internal/navpolicy) answers "may this destination be
// reached at all". It is an operator-configured guardrail with no memory: it
// blocks out-of-policy traffic and keeps no record of the sites a user said yes
// to. This package is the other half - a persistent, per-origin grant with a
// scope, a grantor and an expiry, stored with the profile policy rather than in
// a session, so the answer survives a daemon restart and can be listed, expired
// and revoked.
//
// The store is a file owned by one user, and a file is exactly as trustworthy as
// whatever else runs as that user. Origin Technology found permission entries
// written in plaintext with nothing binding them to the user's consent in
// another agent-browser bridge, which let any local process write itself a grant
// for any domain. Every record here therefore carries a MAC over its own
// authorising fields, keyed by a secret in a 0600 file, and a record whose MAC
// does not verify is discarded rather than honoured.
package siteconsent

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// CanonicalOrigin reduces a URL to the scheme://host[:port] form grants are
// keyed by. Consent is an origin-level decision: a user who allows one page on a
// site has allowed the site, and keying by full URL would ask them again for
// every path.
//
// The host is IDNA-normalised for the same reason navpolicy does it - Chrome
// treats the Unicode and punycode spellings of a name as one site, so two
// spellings of one origin must not become two grant records (one of which the
// user never saw). The default port for the scheme is dropped so "https://x" and
// "https://x:443" are one origin.
//
// A target with no network origin - about:blank, a data: URL, a relative
// reference - returns "" with no error. Callers treat an empty origin as
// "nothing to consent to": there is no site there to grant against.
func CanonicalOrigin(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", nil
	}
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(raw, "//") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	} else if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, "#") {
		// A same-origin relative reference carries no origin of its own. It
		// resolves against whatever page is already open, which has already been
		// consented to, so there is nothing here to ask about.
		return "", nil
	}
	lower := strings.ToLower(raw)
	for _, scheme := range []string{"about:", "data:", "blob:", "javascript:", "vbscript:", "chrome:", "chrome-extension:", "file:", "filesystem:", "view-source:"} {
		if strings.HasPrefix(lower, scheme) {
			return "", nil
		}
	}
	if !strings.Contains(raw, "://") {
		// Bare hosts follow brw's ergonomic https default, matching
		// navpolicy.NormalizeNavigationURL so the origin consented to is the
		// origin that will be opened.
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("cannot read an origin from %q: %w", clip(rawURL), err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", nil
	}
	host := asciiHost(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("cannot read an origin from %q: no host", clip(rawURL))
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}

// HostOfOrigin returns the bare host of a canonical origin, for matching against
// domain lists (which are written as domains, not origins).
func HostOfOrigin(origin string) string {
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return asciiHost(parsed.Hostname())
}

// asciiHost mirrors navpolicy's normalisation: lowercase, strip the root dot and
// convert to punycode, falling back to the lowercased input when IDNA rejects a
// host Chrome would still accept.
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

// hostMatches reports whether host equals domain or is a subdomain of it.
func hostMatches(host, domain string) bool {
	host = asciiHost(host)
	domain = asciiHost(strings.TrimPrefix(strings.TrimPrefix(domain, "*."), "."))
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
