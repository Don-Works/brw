// Package sessionstate seals the session cookies a brw-created browser context
// already holds so a later throwaway context can start signed in.
//
// It is a save/restore pair, deliberately not an export. Nothing here hands a
// stored cookie value to a caller: Store.Load exists only so the restore path
// can push the cookies back into a browser, and Meta — the only shape that
// reaches an MCP result, an HTTP response or a log line — carries origins and
// counts. The security argument is written down in docs/auth-model.md.
package sessionstate

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// Cookie is one sealed cookie. It mirrors the fields CDP needs to put the
// cookie back, and nothing else: a snapshot is not an audit record.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
}

// Snapshot is the sealed payload. It never leaves this package except into the
// restore path.
type Snapshot struct {
	Origins    []string  `json:"origins"`
	Cookies    []Cookie  `json:"cookies"`
	CapturedAt time.Time `json:"captured_at"`
}

// Meta is the redacted view, and the ONLY shape callers outside the restore
// path see. It deliberately has no field that can hold a cookie name or value:
// adding one is the change that would turn brw into the extraction tool
// docs/auth-model.md says it is not.
type Meta struct {
	ID          string    `json:"snapshot_id"`
	Origins     []string  `json:"origins"`
	CookieCount int       `json:"cookie_count"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

var (
	// ErrNoOrigins is the refusal that makes the allowlist mandatory. There is
	// deliberately no "everything" mode: a snapshot always names what it seals.
	ErrNoOrigins = errors.New("origins is required: a session snapshot always names the exact origins it seals, and there is no capture-everything mode")
	// ErrNoRestoreOrigins is what keeps the restore-side filter from checking
	// the file against itself. Defaulting to the origins recorded IN the
	// snapshot would make the second pass a no-op, so the restoring caller
	// always names what it is willing to have installed.
	ErrNoRestoreOrigins = errors.New("origins is required on restore: the allowlist is applied again against the origins the restoring call names, and reading them out of the snapshot would check the file against itself — list the snapshot to see which origins it covers")
	// ErrExpired is returned by Load once a snapshot outlives its TTL. The file
	// is deleted on the way out.
	ErrExpired = errors.New("session snapshot has expired and was deleted; save a new one")
	// ErrNotFound distinguishes an unknown id from an expired one, so a caller
	// can tell a typo from a lapsed session.
	ErrNotFound = errors.New("no session snapshot with that id")
)

// Allowlist is a parsed, exact set of origins. Entries are exact
// scheme://host[:port] origins; a leading-dot or wildcard entry is refused,
// because "allow .test" would sweep every cookie on the suffix.
type Allowlist struct {
	origins []string
	hosts   []string
}

// ParseAllowlist normalizes and validates the caller's origins. A bare host is
// read as https, matching how brw_open treats a scheme-less URL.
func ParseAllowlist(raw []string) (Allowlist, error) {
	if len(raw) == 0 {
		return Allowlist{}, ErrNoOrigins
	}
	seen := map[string]bool{}
	var list Allowlist
	for _, entry := range raw {
		origin, host, err := parseOrigin(entry)
		if err != nil {
			return Allowlist{}, err
		}
		if seen[origin] {
			continue
		}
		seen[origin] = true
		list.origins = append(list.origins, origin)
		list.hosts = append(list.hosts, host)
	}
	if len(list.origins) == 0 {
		return Allowlist{}, ErrNoOrigins
	}
	sort.Strings(list.origins)
	sort.Strings(list.hosts)
	return list, nil
}

func parseOrigin(raw string) (origin, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("empty origin in the allowlist")
	}
	if strings.ContainsAny(raw, "*?[") {
		return "", "", fmt.Errorf("origin %q is a pattern; the session-state allowlist takes exact origins only", raw)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse origin %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("origin %q must be http or https; cookies exist only on http(s) origins", raw)
	}
	host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", fmt.Errorf("origin %q has no host", raw)
	}
	if strings.HasPrefix(host, ".") {
		return "", "", fmt.Errorf("origin %q starts with a dot; the session-state allowlist takes exact origins, not domain suffixes", raw)
	}
	// A single-label host ("https://test") would domain-match every ".test"
	// cookie in the jar, which is the suffix sweep the exact-origin rule exists
	// to prevent. localhost and bare IPs are the legitimate single-label hosts.
	if !strings.Contains(host, ".") && host != "localhost" {
		return "", "", fmt.Errorf("origin %q is a single-label host; name a full host so the allowlist cannot match a whole suffix", raw)
	}
	origin = parsed.Scheme + "://" + host
	if port := parsed.Port(); port != "" {
		origin += ":" + port
	}
	return origin, host, nil
}

// Origins returns the normalized allowlist. Safe to emit: the caller supplied
// these, so echoing them discloses nothing new.
func (a Allowlist) Origins() []string {
	return append([]string(nil), a.origins...)
}

// Empty reports an allowlist that would seal nothing.
func (a Allowlist) Empty() bool { return len(a.hosts) == 0 }

// AllowsCookieDomain applies the ordinary cookie domain-match rule: a cookie
// counts as belonging to an allowlisted origin when its domain is that host, or
// a leading-dot parent of it. Session cookies are routinely set on the
// registrable parent, so exact-host-only matching would seal nothing useful.
func (a Allowlist) AllowsCookieDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return false
	}
	bare := strings.TrimPrefix(domain, ".")
	if bare == "" {
		return false
	}
	// A parent-domain cookie needs at least two labels of its own. Without it
	// a cookie scoped to a bare suffix (".test") would match every allowlisted
	// host under that suffix, which is the sweep the exact-origin rule exists
	// to prevent. This is not a public-suffix list: ".co.uk" would still pass
	// the label count, and the backstop there is that Chrome refuses to store a
	// cookie on a public suffix in the first place.
	parentUsable := strings.HasPrefix(domain, ".") && strings.Contains(bare, ".")
	for _, host := range a.hosts {
		if bare == host {
			return true
		}
		if parentUsable && strings.HasSuffix(host, "."+bare) {
			return true
		}
	}
	return false
}

// Restrict is the single filter both the capture and the restore path run.
// Running it again on restore is what makes the allowlist non-bypassable: a
// snapshot file that somehow carries an off-allowlist cookie still cannot put
// that cookie into a browser, because the restoring caller's own allowlist is
// applied to the decrypted contents.
//
// redact is an extra narrowing on cookie NAMES (glob syntax, path.Match), never
// a widening.
func Restrict(cookies []Cookie, allow Allowlist, redact []string) ([]Cookie, int, error) {
	if allow.Empty() {
		return nil, 0, ErrNoOrigins
	}
	for _, pattern := range redact {
		if _, err := path.Match(pattern, "probe"); err != nil {
			return nil, 0, fmt.Errorf("redact pattern %q is not a valid glob: %w", pattern, err)
		}
	}
	kept := make([]Cookie, 0, len(cookies))
	skipped := 0
	for _, cookie := range cookies {
		if !allow.AllowsCookieDomain(cookie.Domain) {
			skipped++
			continue
		}
		if matchesAny(cookie.Name, redact) {
			skipped++
			continue
		}
		kept = append(kept, cookie)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Domain != kept[j].Domain {
			return kept[i].Domain < kept[j].Domain
		}
		if kept[i].Path != kept[j].Path {
			return kept[i].Path < kept[j].Path
		}
		return kept[i].Name < kept[j].Name
	})
	return kept, skipped, nil
}

func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}
