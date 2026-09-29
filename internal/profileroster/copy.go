package profileroster

import (
	"context"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// CopyDomain copies, or with mode "move" moves, the cookies one site's URL
// scope holds from one brw-owned profile into another, over each profile's own
// daemon. Values pass through this process only; the result carries counts.
//
// Every read and write is an ordinary brw_cookies call on the daemon that owns
// the browser, so each daemon's own site-consent gate and navigation policy
// decide it exactly as they would for an agent.
func CopyDomain(ctx context.Context, policy profilepolicy.Policy, from, to, domain, mode string) (CopyResult, error) {
	domain = RegistrableDomain(domain)
	if domain == "" || strings.ContainsAny(domain, "/:?#@ ") {
		return CopyResult{}, fmt.Errorf("domain must be a bare host such as example.com")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "copy":
		mode = "copy"
	case "move":
	default:
		return CopyResult{}, fmt.Errorf("mode must be copy or move, got %q", mode)
	}
	if from == to {
		return CopyResult{}, fmt.Errorf("source and destination are the same profile")
	}
	src, err := openCookieLane(ctx, policy, from, "source")
	if err != nil {
		return CopyResult{}, err
	}
	dst, err := openCookieLane(ctx, policy, to, "destination")
	if err != nil {
		return CopyResult{}, err
	}
	defer src.release()
	defer dst.release()

	scope := "https://" + domain
	listed, err := src.ctrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: scope})
	if err != nil {
		return CopyResult{}, fmt.Errorf("list %s cookies on %s: %w", domain, from, err)
	}
	var matched []browser.Cookie
	for _, c := range listed.Cookies {
		if RegistrableDomain(c.Domain) == domain {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return CopyResult{}, fmt.Errorf("%s holds no cookies for %s", from, domain)
	}

	result := CopyResult{From: from, To: to, Domain: domain, Mode: mode}
	for _, c := range matched {
		if _, err := dst.ctrl.Cookies(ctx, setParams(c)); err != nil {
			return result, fmt.Errorf("set cookie %q on %s after %d of %d: %w", c.Name, to, result.Copied, len(matched), err)
		}
		result.Copied++
	}

	verify, err := dst.ctrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: scope})
	if err != nil {
		return result, fmt.Errorf("read back %s cookies on %s: %w", domain, to, err)
	}
	present := map[string]bool{}
	for _, c := range verify.Cookies {
		present[cookieKey(c)] = true
	}
	result.Health = HealthCopiedUnverified
	for _, c := range matched {
		if !present[cookieKey(c)] {
			result.Health = HealthMissing
			return result, fmt.Errorf("%s did not keep cookie %q for %s; the source was left untouched", to, c.Name, domain)
		}
	}

	if mode == "move" {
		for _, c := range matched {
			if _, err := src.ctrl.Cookies(ctx, deleteParams(c)); err != nil {
				return result, fmt.Errorf("copied, but removing cookie %q from %s failed after %d of %d: %w", c.Name, from, result.Removed, len(matched), err)
			}
			result.Removed++
		}
	}
	return result, nil
}

func cookieKey(c browser.Cookie) string {
	return strings.TrimPrefix(strings.ToLower(c.Domain), ".") + "|" + c.Path + "|" + c.Name
}

// cookieURL is the URL a cookie is scoped by. A host-only cookie (no leading
// dot) is set through its URL alone, so it stays host-only, which is also what
// a __Host- cookie requires.
func cookieURL(c browser.Cookie) string {
	scheme := "https://"
	if !c.Secure && c.SourceScheme == "NonSecure" {
		scheme = "http://"
	}
	path := c.Path
	if path == "" {
		path = "/"
	}
	return scheme + strings.TrimPrefix(c.Domain, ".") + path
}

func setParams(c browser.Cookie) browser.CookieParams {
	p := browser.CookieParams{
		Action:   browser.CookieActionSet,
		URL:      cookieURL(c),
		Name:     c.Name,
		Value:    c.Value,
		Path:     c.Path,
		Secure:   c.Secure,
		HTTPOnly: c.HTTPOnly,
		SameSite: c.SameSite,
	}
	if strings.HasPrefix(c.Domain, ".") {
		p.Domain = c.Domain
	}
	if !c.Session && c.Expires > 0 {
		p.Expires = c.Expires
	}
	return p
}

func deleteParams(c browser.Cookie) browser.CookieParams {
	p := browser.CookieParams{
		Action: browser.CookieActionDelete,
		URL:    cookieURL(c),
		Name:   c.Name,
		Path:   c.Path,
	}
	if strings.HasPrefix(c.Domain, ".") {
		p.Domain = c.Domain
	}
	return p
}
