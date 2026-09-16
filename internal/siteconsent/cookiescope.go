package siteconsent

import "strings"

// cookieDomainAddressesTheCookie classifies every brw_cookies verb by what its
// `domain` argument means.
//
// For a write the domain IS the cookie's address: the operation names the site
// directly and never consults the tab. For anything else - a list, and any verb
// added later - the domain is a FILTER applied to the cookies the TAB's own
// scope returned, so the tab is a second site the call reaches and has to be
// granted too. Reading the domain and stopping there is how a grant on
// notbank.test returned bank.test's cookies out of a tab nobody looked at.
//
// The map is exhaustive over the verbs brw_cookies accepts, and
// TestEveryCookieActionIsClassified reads them out of CookieParams.Validate and
// fails on one that is not here. A verb missing from it falls to the safe
// answer (the tab is reached) rather than to silence.
var cookieDomainAddressesTheCookie = map[string]bool{
	"list":   false,
	"set":    true,
	"delete": true,
	// An import writes the cookies it was handed, and a declared domain is the
	// address they are written to. With no domain the import falls back to the
	// tab's own URL, which CookieScopeIsTab reports as reaching the tab.
	"import": true,
}

// CookieScopeIsTab reports whether a brw_cookies call resolves its scope from
// the target tab's current URL rather than from its own arguments.
//
// It is exported because the cookie implementation resolves its scope with this
// same function: a gate that decides one thing while the operation does another
// is the whole class of bug this closes, and the only way to stop that drifting
// is for both to read one rule.
func CookieScopeIsTab(rawURL, domain, action string) bool {
	if strings.TrimSpace(rawURL) != "" {
		return false
	}
	if strings.TrimSpace(domain) == "" {
		return true
	}
	addressed, known := cookieDomainAddressesTheCookie[strings.ToLower(strings.TrimSpace(action))]
	return !known || !addressed
}

// cookiesReachTheTab is the ToolRule.PageAlso for brw_cookies.
func cookiesReachTheTab(p Probe) bool {
	return CookieScopeIsTab(p.URL, p.Domain, p.Action)
}
