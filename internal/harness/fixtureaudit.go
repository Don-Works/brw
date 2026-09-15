package harness

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Resource-loading references in a fixture. An <a href> is deliberately absent:
// a link is only fetched if something clicks it, and the fixtures carry
// illustrative links to the open web that no harness ever follows. Everything
// here is fetched by the browser because the page said so.
var resourceRefPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\b(?:src|srcset|data-src|poster|action|formaction)\s*=\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?is)@import\s+(?:url\()?\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?is)\burl\(\s*["']?([^"')]+)["']?\s*\)`),
}

var linkTagPattern = regexp.MustCompile(`(?is)<link\b[^>]*>`)
var attrPattern = regexp.MustCompile(`(?is)\b([a-z-]+)\s*=\s*["']([^"']*)["']`)

// nonFetchingLinkRels are the <link rel> values that declare a relationship
// without making the browser open anything.
//
// The audit is deliberately the other way round from the obvious one: it flags
// every <link href> EXCEPT these, rather than flagging a list of fetching rels.
// The fetching set is open — preconnect, modulepreload, dns-prefetch and
// whatever ships next — and a rel missing from a list of things to flag is a
// silent pass, which is the failure this check exists to prevent.
var nonFetchingLinkRels = map[string]bool{
	"canonical":  true,
	"alternate":  true,
	"author":     true,
	"license":    true,
	"next":       true,
	"prev":       true,
	"search":     true,
	"help":       true,
	"bookmark":   true,
	"me":         true,
	"nofollow":   true,
	"noopener":   true,
	"noreferrer": true,
	"tag":        true,
}

// ExternalResourceRefs returns the references in a fixture that would make the
// browser fetch something off this machine.
//
// The harnesses claim to reach nothing but their own loopback fixture origin.
// That claim is about the pages as much as about the harness: one <img> at a CDN
// makes a "no network" run depend on somebody else's uptime, and it would do so
// silently, as slower numbers rather than as an error.
func ExternalResourceRefs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	var external []string
	seen := map[string]bool{}
	consider := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		if IsExternalRef(ref) {
			external = append(external, ref)
		}
	}
	for _, pattern := range resourceRefPatterns {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			consider(match[1])
		}
	}
	for _, tag := range linkTagPattern.FindAllString(text, -1) {
		rel, href := "", ""
		for _, attr := range attrPattern.FindAllStringSubmatch(tag, -1) {
			switch strings.ToLower(attr[1]) {
			case "rel":
				rel = strings.ToLower(strings.TrimSpace(attr[2]))
			case "href":
				href = attr[2]
			}
		}
		if href == "" || nonFetchingLinkRels[rel] {
			continue
		}
		consider(href)
	}
	return external, nil
}

// IsExternalRef reports whether a reference names a host other than this
// machine. A relative or fragment reference resolves against the fixture origin
// and is not external; a scheme-relative "//host/..." is, which is the form a
// string check for "http" misses.
func IsExternalRef(ref string) bool {
	trimmed := strings.TrimSpace(ref)
	switch {
	case trimmed == "", strings.HasPrefix(trimmed, "#"), strings.HasPrefix(trimmed, "data:"),
		strings.HasPrefix(trimmed, "blob:"), strings.HasPrefix(trimmed, "about:"):
		return false
	case strings.HasPrefix(trimmed, "//"):
		trimmed = "https:" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// An unparseable reference is not something this can clear, and a fixture
		// has no reason to carry one.
		return true
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss", "":
	default:
		return false
	}
	return !isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// AuditFixtures checks several fixtures at once and returns one error naming
// every offending reference.
func AuditFixtures(paths []string) error {
	var problems []string
	for _, path := range paths {
		refs, err := ExternalResourceRefs(path)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			problems = append(problems, fmt.Sprintf("%s loads %s", path, ref))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("fixtures reach off this machine: %s", strings.Join(problems, "; "))
}
