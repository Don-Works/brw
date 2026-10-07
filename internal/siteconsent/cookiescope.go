package siteconsent

import "strings"

var cookieDomainAddressesTheCookie = map[string]bool{
	"list":   false,
	"set":    true,
	"delete": true,

	"import": true,
}

// CookieScopeIsTab reports whether a brw_cookies call resolves its scope from the target tab's current URL rather than from its own arguments.
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

func cookiesReachTheTab(p Probe) bool {
	return CookieScopeIsTab(p.URL, p.Domain, p.Action)
}
