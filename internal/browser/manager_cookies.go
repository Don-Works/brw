package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// CookieParams is the shared request shape for brw_cookies across transports.
// Action selects the verb: "list" (read cookies applicable to a URL, including
// HttpOnly ones page JS cannot see), "set" (write a cookie with full attribute
// control), or "delete" (remove matching cookies). URL scopes the operation to
// an origin; when omitted the target tab's current URL is used, so cookies
// naturally follow the tab an agent is driving.
type CookieParams struct {
	Action   string  `json:"action"`
	URL      string  `json:"url,omitempty"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Name     string  `json:"name,omitempty"`
	Value    string  `json:"value,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// Cookie is one browser cookie as reported by CDP Network.getCookies. Value is
// included deliberately: brw_cookies is the explicit, audited cookie surface
// (direct-CDP transports only), unlike the extension bridge whose cookie access
// is blocked by policy.
type Cookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	Size         int64   `json:"size"`
	HTTPOnly     bool    `json:"http_only"`
	Secure       bool    `json:"secure"`
	Session      bool    `json:"session"`
	SameSite     string  `json:"same_site,omitempty"`
	Priority     string  `json:"priority,omitempty"`
	SourceScheme string  `json:"source_scheme,omitempty"`
	SourcePort   int64   `json:"source_port,omitempty"`
}

// CookieResult is what brw_cookies returns. List carries the full applicable
// set under cookies; set reads the stored cookie back under cookie (so callers
// see the domain/path Chrome actually persisted); delete reports how many
// cookies with the same name remain applicable after the deletion.
type CookieResult struct {
	Action string `json:"action"`
	URL    string `json:"url,omitempty"`
	// Cookies is always present (possibly empty) so agents reading
	// result.cookies never have to distinguish "zero cookies" from "unknown".
	Cookies []Cookie `json:"cookies"`
	Count   int      `json:"count"`
	Cookie  *Cookie  `json:"cookie,omitempty"`
	// RemainingSameName is the post-delete sanity check: cookies still
	// applicable to the scope carrying the deleted name. Zero is the expected
	// outcome; a non-zero value means the delete matched a narrower
	// domain/path than the caller assumed.
	RemainingSameName int `json:"remaining_same_name,omitempty"`
}

// CookieActions are the verbs brw_cookies accepts.
const (
	CookieActionList   = "list"
	CookieActionSet    = "set"
	CookieActionDelete = "delete"
)

// Validate checks the action-specific required fields before any browser I/O,
// so a malformed request fails fast with a precise message on every transport.
func (p CookieParams) Validate() error {
	switch strings.ToLower(strings.TrimSpace(p.Action)) {
	case CookieActionList:
		return nil
	case CookieActionSet:
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("name is required for set")
		}
	case CookieActionDelete:
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("name is required for delete")
		}
	case "":
		return errors.New(`action is required: "list", "set", or "delete"`)
	default:
		return fmt.Errorf("unknown action %q: want list, set, or delete", p.Action)
	}
	return nil
}

// cookieScopeURL normalizes the URL a cookie operation is scoped to. A bare
// host ("example.com" or "example.com/path") is treated as https, matching how
// brw_open treats scheme-less URLs; a full URL is used as-is. The returned
// value is what CDP Network.getCookies/Network.deleteCookies match against.
func cookieScopeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty cookie scope url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse cookie scope url: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("cookie scope url %q has no host", raw)
	}
	// Cookies are meaningless (and Chrome refuses to store them) for non-http(s)
	// schemes like file: and about:; catch that before a confusing CDP failure.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("cookies require an http(s) origin, got scheme %q (file:// and about:blank pages cannot hold cookies)", parsed.Scheme)
	}
	return raw, nil
}

// tabScopeURL resolves the fallback scope: the target tab's current URL. A tab
// that never navigated (about:blank) has no cookie origin, so the error tells
// the caller to pass url explicitly.
func (m *Manager) tabScopeURL(ctx context.Context, tabID string) (string, error) {
	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		return "", err
	}
	if tab.URL == "" || strings.HasPrefix(tab.URL, "about:blank") {
		return "", errors.New("the target tab has no navigated origin (about:blank); pass url explicitly for cookie operations")
	}
	return tab.URL, nil
}

// sameSiteFromUser maps the tool's friendly same_site values to CDP constants.
// An empty value means "let Chrome default" (Lax for modern Chrome) and is left
// unset. An unrecognized value is an error, not a silent default.
func sameSiteFromUser(v string) (network.CookieSameSite, bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", false, nil
	case "strict":
		return network.CookieSameSiteStrict, true, nil
	case "lax":
		return network.CookieSameSiteLax, true, nil
	case "none":
		return network.CookieSameSiteNone, true, nil
	default:
		return "", false, fmt.Errorf("unknown same_site %q: want strict, lax, or none", v)
	}
}

// domainMatches reports whether a stored cookie domain applies to the scope:
// either the exact host, or a registrable parent (a leading-dot ".example.com"
// cookie matches "shop.example.com").
//
// The suffix is anchored on the dot. A bare strings.HasSuffix made ".bank.test"
// match "notbank.test", which is a different site that happens to end in the
// same letters, and the filter is what decides whose cookies come back.
func cookieDomainMatches(cookieDomain, host string) bool {
	cookieDomain = strings.ToLower(strings.TrimSpace(cookieDomain))
	host = strings.ToLower(strings.TrimSpace(host))
	trimmed := strings.TrimPrefix(cookieDomain, ".")
	if trimmed == "" || host == "" {
		return false
	}
	if cookieDomain == host || trimmed == host {
		return true
	}
	return strings.HasPrefix(cookieDomain, ".") && strings.HasSuffix(host, "."+trimmed)
}

// fromCDPCookie converts the cdproto wire cookie into the stable tool shape
// (snake_case JSON, string enums).
func fromCDPCookie(c *network.Cookie) Cookie {
	out := Cookie{
		Name:         c.Name,
		Value:        c.Value,
		Domain:       c.Domain,
		Path:         c.Path,
		Expires:      c.Expires,
		Size:         c.Size,
		HTTPOnly:     c.HTTPOnly,
		Secure:       c.Secure,
		Session:      c.Session,
		SameSite:     string(c.SameSite),
		Priority:     string(c.Priority),
		SourceScheme: string(c.SourceScheme),
		SourcePort:   c.SourcePort,
	}
	return out
}

// listCookiesForURL reads the cookies applicable to scopeURL on the tab's CDP
// session — the shared read behind list, the post-set read-back, and the
// post-delete verification. Optional exact name/domain/path filters narrow it.
func (m *Manager) listCookiesForURL(tabCtx context.Context, params CookieParams, scopeURL string) ([]Cookie, error) {
	// The CDP call runs inside chromedp.Run so the target-session executor is
	// bound — a bare .Do(tabCtx) has no executor until Run attaches one.
	var cdCookies []*network.Cookie
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cdCookies, err = network.GetCookies().WithURLs([]string{scopeURL}).Do(ctx)
		return err
	})); err != nil {
		return nil, fmt.Errorf("Network.getCookies: %w", err)
	}
	wantDomain := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(params.Domain, ".")))
	wantName := strings.TrimSpace(params.Name)
	wantPath := strings.TrimSpace(params.Path)
	out := make([]Cookie, 0, len(cdCookies))
	for _, c := range cdCookies {
		if wantName != "" && c.Name != wantName {
			continue
		}
		if wantDomain != "" && !cookieDomainMatches(c.Domain, wantDomain) {
			continue
		}
		if wantPath != "" && c.Path != wantPath {
			continue
		}
		out = append(out, fromCDPCookie(c))
	}
	return out, nil
}

// Cookies implements brw_cookies on the direct-CDP transport via the CDP
// Network cookie commands — the same store the tab's requests use, so HttpOnly
// cookies are visible and settable, unlike document.cookie. The extension
// bridge intentionally refuses this operation (its security policy blocks
// cookie CDP methods to protect the signed-in profile).
func (m *Manager) Cookies(ctx context.Context, params CookieParams) (CookieResult, error) {
	if err := params.Validate(); err != nil {
		return CookieResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return CookieResult{}, err
	}
	defer cancel()

	// Resolve the scope: explicit url wins, else the tab's current origin. For
	// set/delete an explicit domain may substitute for a URL. The predicate is
	// shared with the consent gate so the gate cannot check one site while the
	// operation reads another.
	scopeURL := strings.TrimSpace(params.URL)
	if !siteconsent.CookieScopeIsTab(params.URL, params.Domain, params.Action) && scopeURL == "" {
		// Domain-scoped set/delete: build a synthetic scope URL for the verification
		// read. The cookie itself is addressed by domain/path directly; the scope
		// only decides which stored cookies getCookies returns. Loopback hosts keep
		// http unless secure was asked for, matching how Chrome treats them.
		scheme := "https"
		host := strings.TrimPrefix(strings.TrimSpace(params.Domain), ".")
		if !params.Secure && (strings.Contains(host, "localhost") || strings.HasPrefix(host, "127.")) {
			scheme = "http"
		}
		scopeURL = scheme + "://" + host + "/"
	}
	if scopeURL == "" {
		scopeURL, err = m.tabScopeURL(ctx, tabID)
		if err != nil {
			return CookieResult{}, err
		}
	}
	scopeURL, err = cookieScopeURL(scopeURL)
	if err != nil {
		return CookieResult{}, err
	}

	action := strings.ToLower(strings.TrimSpace(params.Action))
	switch action {
	case CookieActionList:
		cookies, err := m.listCookiesForURL(tabCtx, params, scopeURL)
		if err != nil {
			return CookieResult{}, err
		}
		if cookies == nil {
			// A stable shape matters more than two empty-list bytes: agents read
			// result.cookies unconditionally, and an omitted key reads as "unknown"
			// rather than "zero cookies".
			cookies = []Cookie{}
		}
		return CookieResult{Action: action, URL: scopeURL, Cookies: cookies, Count: len(cookies)}, nil

	case CookieActionSet:
		sameSite, setSameSite, err := sameSiteFromUser(params.SameSite)
		if err != nil {
			return CookieResult{}, err
		}
		cmd := network.SetCookie(params.Name, params.Value)
		if strings.TrimSpace(params.Domain) != "" {
			cmd = cmd.WithDomain(strings.TrimSpace(params.Domain))
			cmd = cmd.WithPath(cookiePathOrDefault(params.Path))
		} else {
			cmd = cmd.WithURL(scopeURL)
			if strings.TrimSpace(params.Path) != "" {
				cmd = cmd.WithPath(strings.TrimSpace(params.Path))
			}
		}
		if params.Secure {
			cmd = cmd.WithSecure(true)
		}
		if params.HTTPOnly {
			cmd = cmd.WithHTTPOnly(true)
		}
		if setSameSite {
			cmd = cmd.WithSameSite(sameSite)
		}
		if params.Expires > 0 {
			exp := cdp.TimeSinceEpoch(time.Unix(int64(params.Expires), 0))
			cmd = cmd.WithExpires(&exp)
		}
		// NOTE: Network.setCookie's success flag is unreliable on current Chrome
		// (it returns false for cookies it demonstrably stores), so the command's
		// transport error is the only failure honored here; acceptance is proven
		// by reading the cookie back from the jar.
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return cdp.Execute(ctx, network.CommandSetCookie, cmd, &struct {
				Success bool `json:"success"`
			}{})
		})); err != nil {
			return CookieResult{}, fmt.Errorf("Network.setCookie: %w", err)
		}
		// Read the stored cookie back so the caller sees the domain/path Chrome
		// actually persisted, not what it asked for — and so a genuinely refused
		// cookie (e.g. Secure-required SameSite=None) surfaces as an error.
		stored, err := m.listCookiesForURL(tabCtx, CookieParams{Name: params.Name, Domain: params.Domain, Path: params.Path}, scopeURL)
		if err != nil {
			return CookieResult{}, err
		}
		if len(stored) == 0 {
			return CookieResult{}, fmt.Errorf("chrome refused to store cookie %q for %s (check secure/same_site constraints: same_site none requires secure, and secure cookies require an https origin)", params.Name, scopeURL)
		}
		return CookieResult{Action: action, URL: scopeURL, Cookie: &stored[0], Cookies: stored}, nil

	case CookieActionDelete:
		cmd := network.DeleteCookies(params.Name)
		if strings.TrimSpace(params.Domain) != "" {
			cmd = cmd.WithDomain(strings.TrimSpace(params.Domain))
			cmd = cmd.WithPath(cookiePathOrDefault(params.Path))
		} else {
			cmd = cmd.WithURL(scopeURL)
			if strings.TrimSpace(params.Path) != "" {
				cmd = cmd.WithPath(strings.TrimSpace(params.Path))
			}
		}
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return cmd.Do(ctx)
		})); err != nil {
			return CookieResult{}, fmt.Errorf("Network.deleteCookies: %w", err)
		}
		remaining, err := m.listCookiesForURL(tabCtx, CookieParams{Name: params.Name}, scopeURL)
		if err != nil {
			// Deletion succeeded; the verification read is best-effort.
			return CookieResult{Action: action, URL: scopeURL, Cookies: []Cookie{}}, nil
		}
		return CookieResult{Action: action, URL: scopeURL, RemainingSameName: len(remaining), Cookies: []Cookie{}}, nil
	}
	return CookieResult{}, fmt.Errorf("unknown action %q", params.Action)
}

// cookiePathOrDefault applies the cookie default path ("/") when a domain-
// scoped operation omits one — CDP requires path together with domain.
func cookiePathOrDefault(p string) string {
	if p = strings.TrimSpace(p); p == "" {
		return "/"
	}
	return p
}

// CookieMeta is a cookie with the secret stripped. The profile manager and any
// other operator UI must use this, never Cookie, so values cannot leak into HTML.
type CookieMeta struct {
	Name     string  `json:"name"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
	Session  bool    `json:"session"`
	SameSite string  `json:"same_site,omitempty"`
}

// Meta returns the non-secret fields of c.
func (c Cookie) Meta() CookieMeta {
	return CookieMeta{
		Name:     c.Name,
		Domain:   c.Domain,
		Path:     c.Path,
		Expires:  c.Expires,
		HTTPOnly: c.HTTPOnly,
		Secure:   c.Secure,
		Session:  c.Session,
		SameSite: c.SameSite,
	}
}

// AllCookies returns every cookie in this browser's jar via Network.getAllCookies.
// Direct-CDP only. Used by the profile manager to inventory domains; callers
// must convert to CookieMeta before crossing into a UI.
func (m *Manager) AllCookies(ctx context.Context) ([]Cookie, error) {
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	var result struct {
		Cookies []*network.Cookie `json:"cookies"`
	}
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		// Generated chromedp dropped GetAllCookies; the CDP method still exists
		// on current Chrome as Network.getAllCookies.
		return cdp.Execute(ctx, "Network.getAllCookies", nil, &result)
	})); err != nil {
		return nil, fmt.Errorf("Network.getAllCookies: %w", err)
	}
	cdCookies := result.Cookies
	out := make([]Cookie, 0, len(cdCookies))
	for _, c := range cdCookies {
		out = append(out, fromCDPCookie(c))
	}
	return out, nil
}
