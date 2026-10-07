// Package siteconsent records what a user actually allowed brw to do on a site, so there is something to show them and something to revoke.
package siteconsent

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// CanonicalOrigin reduces a URL to the scheme://host[:port] form grants are keyed by.
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
		return "", nil
	}
	lower := strings.ToLower(raw)
	for _, scheme := range inertSchemes {
		if strings.HasPrefix(lower, scheme) {
			return "", nil
		}
	}
	for _, scheme := range localSchemes {
		if strings.HasPrefix(lower, scheme) {
			return "", nil
		}
	}
	if !strings.Contains(raw, "://") {
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
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

var inertSchemes = []string{"about:", "data:", "blob:"}

var localSchemes = []string{"file:", "filesystem:", "view-source:", "chrome:", "chrome-extension:", "javascript:", "vbscript:"}

// LocalScheme reports whether a target is one of the local or privileged schemes and names it.
func LocalScheme(rawURL string) (string, bool) {
	raw := strings.TrimSpace(rawURL)
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	raw = strings.ToLower(strings.ReplaceAll(raw, "\\", "/"))
	for _, scheme := range localSchemes {
		if strings.HasPrefix(raw, scheme) {
			return strings.TrimSuffix(scheme, ":"), true
		}
	}
	return "", false
}

// HostOfOrigin returns the bare host of a canonical origin, for matching against domain lists (which are written as domains, not origins).
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
