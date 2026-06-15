package browser

import (
	"errors"
	"net/url"
	"strings"
	"sync"
)

// ErrBlockedByPolicy is returned when an action is refused by the session
// policy layer. Callers surface it as an explicit error so the operator sees a
// hard block rather than a silent no-op.
var ErrBlockedByPolicy = errors.New("action blocked by browser policy")

// PolicySettings is the wire representation of the session consent/safety
// envelope. It is intentionally open by default: an empty Allow list means every
// origin is permitted (the open-web stance), and the purchase gate defaults to
// enforced so checkout/payment/place-order actions require explicit per-session
// authorization before they run.
//
// The zero value (PurchaseGate false, everything empty) is NOT the default —
// DefaultPolicySettings constructs the safe default with the purchase gate on.
type PolicySettings struct {
	// PurchaseGate, when true, blocks purchase/payment/place-order style actions
	// unless PurchaseAuthorized is also true. Defaults to true.
	PurchaseGate bool `json:"purchase_gate"`
	// PurchaseAuthorized is the explicit per-session authorization that unlocks
	// the purchase gate. Defaults to false; the operator opts in via browser_policy.
	PurchaseAuthorized bool `json:"purchase_authorized"`
	// Allow is an optional opt-in per-origin allow list. When non-empty, ONLY
	// origins on this list may be acted on (every other origin is denied).
	// Empty (the default) means open: all origins allowed.
	Allow []string `json:"allow,omitempty"`
	// Deny is an optional opt-in per-origin deny list. Origins on this list are
	// always blocked, even if they also appear on Allow. Defaults to empty.
	Deny []string `json:"deny,omitempty"`
}

// DefaultPolicySettings returns the safe-by-default envelope: open web (no
// origin allow/deny lists) with the purchase gate enforced and unauthorized.
func DefaultPolicySettings() PolicySettings {
	return PolicySettings{PurchaseGate: true}
}

// normalize lower-cases and trims the origin lists so comparisons are stable.
func (s PolicySettings) normalize() PolicySettings {
	s.Allow = normalizeOrigins(s.Allow)
	s.Deny = normalizeOrigins(s.Deny)
	return s
}

func normalizeOrigins(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		host := NormalizeOriginHost(raw)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PolicyStore is the per-session holder for the consent/safety envelope. It is
// safe for concurrent use; the Manager (and Bridge) embed one.
type PolicyStore struct {
	mu       sync.RWMutex
	settings PolicySettings
}

// NewPolicyStore returns a store initialized with the safe default envelope.
func NewPolicyStore() *PolicyStore {
	return &PolicyStore{settings: DefaultPolicySettings()}
}

// Get returns a copy of the current settings.
func (p *PolicyStore) Get() PolicySettings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := p.settings
	// Return defensive copies of the slices so callers cannot mutate state.
	if len(s.Allow) > 0 {
		s.Allow = append([]string(nil), s.Allow...)
	}
	if len(s.Deny) > 0 {
		s.Deny = append([]string(nil), s.Deny...)
	}
	return s
}

// Set replaces the settings (after normalization) and returns the stored copy.
func (p *PolicyStore) Set(s PolicySettings) PolicySettings {
	s = s.normalize()
	p.mu.Lock()
	p.settings = s
	p.mu.Unlock()
	return p.Get()
}

// PolicyDecision is the result of evaluating a candidate action against the
// session policy. Blocked actions carry a machine-stable Category and a human
// Reason; non-blocking advisories (e.g. a purchase-like action that has been
// authorized) carry a Warning instead.
type PolicyDecision struct {
	Blocked  bool
	Category string
	Reason   string
	Warning  string
}

// Error returns ErrBlockedByPolicy wrapped with the decision reason, suitable
// for returning directly from a Controller method.
func (d PolicyDecision) Error() error {
	if !d.Blocked {
		return nil
	}
	reason := d.Reason
	if reason == "" {
		reason = "policy denied"
	}
	return errors.Join(ErrBlockedByPolicy, errors.New(reason))
}

// EvaluateActionPolicy decides whether an action targeting the given page URL
// (currentURL) and control (label/href) is permitted under the settings.
//
// Order of checks (most-restrictive first):
//  1. Per-origin deny list — hard block.
//  2. Per-origin allow list (when non-empty) — block anything not listed.
//  3. Purchase/payment/place-order gate — block unless authorized.
//
// Everything else is allowed (open web preserved).
func EvaluateActionPolicy(settings PolicySettings, currentURL, label, href string) PolicyDecision {
	host := actionHost(currentURL, href)

	if host != "" {
		for _, denied := range settings.Deny {
			if originMatches(host, denied) {
				return PolicyDecision{
					Blocked:  true,
					Category: "origin_denied",
					Reason:   "origin " + host + " is on the policy deny list",
				}
			}
		}
		if len(settings.Allow) > 0 {
			allowed := false
			for _, ok := range settings.Allow {
				if originMatches(host, ok) {
					allowed = true
					break
				}
			}
			if !allowed {
				return PolicyDecision{
					Blocked:  true,
					Category: "origin_not_allowed",
					Reason:   "origin " + host + " is not on the policy allow list",
				}
			}
		}
	}

	if kind := classifyPurchaseAction(label, href); kind != "" {
		warning := purchaseWarning(kind)
		if settings.PurchaseGate && !settings.PurchaseAuthorized {
			return PolicyDecision{
				Blocked:  true,
				Category: "purchase_gate",
				Reason:   warning + " (blocked: set browser_policy purchase_authorized=true to allow)",
			}
		}
		// Gate disabled or explicitly authorized: allow but surface the advisory.
		return PolicyDecision{Warning: warning}
	}

	return PolicyDecision{}
}

// classifyPurchaseAction unifies the purchase/payment detection that previously
// lived in PurchaseControlWarning. It returns a non-empty category for a
// purchase-like control and "" otherwise. Generic, keyword-based, no
// site-specific logic.
func classifyPurchaseAction(label, href string) string {
	combined := strings.ToLower(strings.TrimSpace(label + " " + href))
	if combined == "" {
		return ""
	}
	for _, phrase := range purchaseCommitPhrases {
		if strings.Contains(combined, phrase) {
			return "purchase_commit"
		}
	}
	for _, phrase := range checkoutPhrases {
		if strings.Contains(combined, phrase) {
			return "checkout"
		}
	}
	return ""
}

var purchaseCommitPhrases = []string{
	"place order",
	"place your order",
	"submit order",
	"confirm order",
	"confirm purchase",
	"confirm payment",
	"confirm and pay",
	"pay now",
	"pay and",
	"complete purchase",
	"complete order",
	"buy now",
	"purchase now",
	"proceed to payment",
	"authorize payment",
}

var checkoutPhrases = []string{
	"checkout",
	"check out",
	"proceed to checkout",
}

func purchaseWarning(kind string) string {
	switch kind {
	case "purchase_commit":
		return "purchase/payment control detected; placing an order or paying requires explicit user authorization"
	case "checkout":
		return "checkout navigation detected; stop before payment or place-order controls unless explicitly authorized"
	default:
		return "purchase-sensitive control detected"
	}
}

// PurchaseControlWarning is retained for backward compatibility with existing
// call sites: it returns the advisory string for a purchase-like control, or ""
// for anything else. The enforceable gate now lives in EvaluateActionPolicy.
func PurchaseControlWarning(label, href string) string {
	if kind := classifyPurchaseAction(label, href); kind != "" {
		return purchaseWarning(kind)
	}
	return ""
}

// actionHost resolves the host an action affects. The control href (when it is
// an absolute, non-fragment navigation) takes precedence because it is where
// the click will land; otherwise the current page URL is used.
func actionHost(currentURL, href string) string {
	if h := hostFromHref(href); h != "" {
		return h
	}
	return NormalizeOriginHost(currentURL)
}

func hostFromHref(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "/") {
		return ""
	}
	if !strings.Contains(href, "://") {
		return ""
	}
	return NormalizeOriginHost(href)
}

// NormalizeOriginHost extracts a bare lower-cased host from a URL or host
// string, stripping scheme, port, userinfo, and path. Returns "" for blank or
// unparseable input.
func NormalizeOriginHost(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	// Bare host (possibly host:port or with a trailing path).
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// originMatches reports whether host matches a configured origin entry. An entry
// matches its exact host and any subdomain of it (so "example.com" covers
// "www.example.com"). Comparison is registrable-domain aware only via suffix
// matching; both inputs are assumed pre-normalized to bare hosts.
func originMatches(host, entry string) bool {
	host = NormalizeOriginHost(host)
	entry = NormalizeOriginHost(entry)
	if host == "" || entry == "" {
		return false
	}
	if host == entry {
		return true
	}
	return strings.HasSuffix(host, "."+entry)
}

// RegistrableDomain returns a best-effort registrable ("eTLD+1") domain for a
// host without an external public-suffix dataset. It handles plain TLDs
// (example.com -> example.com) and the common two-label public suffixes
// (foo.co.uk -> foo.co.uk) generically. This is used only for the advisory
// cross-domain transition flag, so a heuristic is acceptable.
func RegistrableDomain(raw string) string {
	host := NormalizeOriginHost(raw)
	if host == "" {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	// If the last two labels form a known two-part public suffix, keep three
	// labels; otherwise keep the last two.
	lastTwo := labels[len(labels)-2] + "." + labels[len(labels)-1]
	if _, ok := twoPartPublicSuffixes[lastTwo]; ok {
		return strings.Join(labels[len(labels)-3:], ".")
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// twoPartPublicSuffixes is a small, generic set of the most common multi-label
// public suffixes so RegistrableDomain does not mistake "foo.co.uk" for
// "co.uk". Not exhaustive (no embedded PSL), and intentionally vendor-neutral.
var twoPartPublicSuffixes = map[string]struct{}{
	"co.uk":  {},
	"org.uk": {},
	"gov.uk": {},
	"ac.uk":  {},
	"co.jp":  {},
	"co.kr":  {},
	"co.nz":  {},
	"co.za":  {},
	"co.in":  {},
	"com.au": {},
	"net.au": {},
	"org.au": {},
	"com.br": {},
	"com.cn": {},
	"com.mx": {},
	"com.tr": {},
	"com.sg": {},
	"com.hk": {},
}

// CrossDomainTransition reports whether navigating from beforeURL to afterURL
// crossed into a new registrable domain. A blank before or after URL, or
// identical registrable domains, returns false.
func CrossDomainTransition(beforeURL, afterURL string) (string, bool) {
	beforeDomain := RegistrableDomain(beforeURL)
	afterDomain := RegistrableDomain(afterURL)
	if beforeDomain == "" || afterDomain == "" {
		return "", false
	}
	if beforeDomain == afterDomain {
		return "", false
	}
	return afterDomain, true
}
