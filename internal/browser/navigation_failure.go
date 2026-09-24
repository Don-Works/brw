package browser

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// AuthChallenge is the first HTTP authentication challenge a server sent with a
// 401 (WWW-Authenticate) or 407 (Proxy-Authenticate) response.
type AuthChallenge struct {
	Scheme string `json:"scheme"`
	Realm  string `json:"realm,omitempty"`
}

// BrowserAnswerable reports whether the challenge is one the browser itself
// answers with a stored credential, which is what brw_authenticate supplies.
func (c AuthChallenge) BrowserAnswerable() bool {
	switch strings.ToLower(c.Scheme) {
	case "basic", "digest", "ntlm", "negotiate":
		return true
	}
	return false
}

// NavigationOutcome is what brw learned about one top-level navigation beyond
// the fact that a document committed.
type NavigationOutcome struct {
	// URL is the destination brw asked for.
	URL string
	// CommittedURL is the main frame's URL after the navigation committed.
	CommittedURL string
	// Error is the network error Chrome reported, such as net::ERR_NAME_NOT_RESOLVED.
	Error        string
	HTTPStatus   int
	AuthRequired *AuthChallenge
}

// ErrorPageURL is the URL Chrome commits when a navigation fails and it shows
// its own error page instead of the destination.
const ErrorPageURL = "chrome-error://chromewebdata/"

// IsErrorPageURL reports whether a committed main-frame URL is Chrome's error page.
func IsErrorPageURL(u string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "chrome-error:")
}

// navigationFailedMarker is the phrase every failed-navigation message carries,
// so a caller that only sees the text (an HTTP upstream, the usage log) can
// still classify it.
const navigationFailedMarker = "navigation failed:"

// IsNavigationFailedMessage reports whether an error text is a failed-navigation report.
func IsNavigationFailedMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), navigationFailedMarker)
}

// Failed reports whether the navigation did not reach the destination: Chrome
// committed its error page, or reported a network error. net::ERR_ABORTED is
// excluded because it leaves the previous document in place and has its own
// message (NavigationAbortedError).
func (o NavigationOutcome) Failed() bool {
	if IsErrorPageURL(o.CommittedURL) {
		return true
	}
	e := strings.TrimSpace(o.Error)
	return e != "" && !strings.Contains(e, "net::ERR_ABORTED")
}

func (o NavigationOutcome) authChallengeImplied() bool {
	if o.AuthRequired != nil {
		return true
	}
	return o.HTTPStatus == 401 || o.HTTPStatus == 407 ||
		strings.Contains(o.Error, "ERR_INVALID_AUTH_CREDENTIALS")
}

// Describe renders a failed navigation for an agent: what happened, and what
// to do about it. authAvailable says whether this transport offers
// brw_authenticate.
func (o NavigationOutcome) Describe(verb string, authAvailable bool) string {
	var b strings.Builder
	b.WriteString(verb)
	b.WriteString(": ")
	b.WriteString(navigationFailedMarker)
	b.WriteString(" ")
	target := o.URL
	if target == "" {
		target = "the destination"
	}
	b.WriteString(target)
	if o.HTTPStatus > 0 {
		fmt.Fprintf(&b, " answered HTTP %d", o.HTTPStatus)
	} else {
		b.WriteString(" did not load")
	}
	if o.Error != "" {
		fmt.Fprintf(&b, " (%s)", o.Error)
	}
	if IsErrorPageURL(o.CommittedURL) {
		fmt.Fprintf(&b, "; the tab shows Chrome's error page (%s), not the site", strings.TrimSpace(o.CommittedURL))
	}
	b.WriteString(".")
	if !o.authChallengeImplied() {
		return b.String()
	}
	challenge := AuthChallenge{}
	if o.AuthRequired != nil {
		challenge = *o.AuthRequired
		fmt.Fprintf(&b, " The server asks for HTTP %s authentication", challenge.Scheme)
		if challenge.Realm != "" {
			fmt.Fprintf(&b, " (realm %q)", challenge.Realm)
		}
		b.WriteString(".")
	} else {
		b.WriteString(" The server asks for HTTP authentication.")
	}
	if o.AuthRequired != nil && !challenge.BrowserAnswerable() {
		fmt.Fprintf(&b, " A %s challenge is answered by the site's own sign-in flow, not by the browser; sign in through the page, or ask the user for access.", challenge.Scheme)
		return b.String()
	}
	if authAvailable {
		b.WriteString(" Call brw_authenticate with the credentials for this origin and url; it loads the page with the challenge answered.")
	} else {
		b.WriteString(" brw_authenticate is not available on the extension bridge, which cannot answer an HTTP authentication challenge. Ask the user to switch this work to a CDP lane (direct-cdp, or chrome-opt-in-cdp to keep the signed-in Chrome) and call brw_authenticate there.")
	}
	return b.String()
}

// ApplyNavigationOutcome copies what brw learned about the open's navigation
// onto the result, and marks a failed navigation not ready.
func (r *OpenResult) ApplyNavigationOutcome(o NavigationOutcome, authAvailable bool) {
	if o.HTTPStatus >= 400 {
		r.HTTPStatus = o.HTTPStatus
	}
	if o.AuthRequired != nil {
		challenge := *o.AuthRequired
		r.AuthRequired = &challenge
	}
	if !o.Failed() {
		return
	}
	r.Ready = false
	r.NavigationError = strings.TrimSpace(o.Error)
	if r.NavigationError == "" {
		r.NavigationError = "chrome-error page (the browser reported no error code)"
	}
	r.Warning = o.Describe("open", authAvailable)
}

// NavigationErr is the open's failed navigation as an error, for a caller that
// has to stop on it; nil when the navigation did not fail. The tab exists either
// way, so the message names it for the caller to close.
func (r OpenResult) NavigationErr() error {
	if r.NavigationError == "" {
		return nil
	}
	message := r.Warning
	if r.Tab.ID != "" {
		message += " The tab stays open as tab_id " + r.Tab.ID + "; close it with brw_close_tab when you are done with it."
	}
	return errors.New(message)
}

// NavigationFailedError is a navigation that ended on Chrome's error page or a
// network error, carried as an error by calls that have no result to hold it.
type NavigationFailedError struct {
	Verb          string
	Outcome       NavigationOutcome
	AuthAvailable bool
}

func (e *NavigationFailedError) Error() string {
	return e.Outcome.Describe(e.Verb, e.AuthAvailable)
}

var (
	authSchemeRE = regexp.MustCompile(`^\s*([A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+)`)
	authRealmRE  = regexp.MustCompile(`(?i)(?:^|[\s,])realm\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^\s,]+))`)
)

// ParseAuthChallenge reads the first challenge from WWW-Authenticate (or
// Proxy-Authenticate) header values. When several are offered it prefers one
// the browser can answer itself (Basic, Digest, NTLM, Negotiate), since that is
// the one brw_authenticate can satisfy. It returns nil when no value names a
// scheme.
func ParseAuthChallenge(values []string) *AuthChallenge {
	var first *AuthChallenge
	for _, value := range values {
		for _, candidate := range splitAuthChallenges(value) {
			challenge := parseOneChallenge(candidate)
			if challenge == nil {
				continue
			}
			if challenge.BrowserAnswerable() {
				return challenge
			}
			if first == nil {
				first = challenge
			}
		}
	}
	return first
}

func parseOneChallenge(value string) *AuthChallenge {
	match := authSchemeRE.FindStringSubmatch(value)
	if match == nil {
		return nil
	}
	challenge := &AuthChallenge{Scheme: canonicalAuthScheme(match[1])}
	rest := value[len(match[0]):]
	if realm := authRealmRE.FindStringSubmatch(rest); realm != nil {
		if realm[1] != "" {
			challenge.Realm = strings.ReplaceAll(realm[1], `\"`, `"`)
		} else {
			challenge.Realm = realm[2]
		}
	}
	return challenge
}

// splitAuthChallenges splits one header value that lists several challenges
// ("Bearer realm=\"a\", Basic realm=\"b\"") at each new scheme token. A comma
// followed by a bare token and a space (or the end) starts a new challenge; a
// comma followed by name=value continues the current one's parameters.
func splitAuthChallenges(value string) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			if inQuote {
				i++
			}
		case '"':
			inQuote = !inQuote
		case ',':
			if inQuote {
				continue
			}
			if startsChallenge(value[i+1:]) {
				out = append(out, value[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, value[start:])
	return out
}

func startsChallenge(rest string) bool {
	trimmed := strings.TrimLeft(rest, " \t")
	match := authSchemeRE.FindString(trimmed)
	if match == "" {
		return false
	}
	after := strings.TrimLeft(trimmed[len(match):], " \t")
	return !strings.HasPrefix(after, "=")
}

func canonicalAuthScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "basic":
		return "Basic"
	case "digest":
		return "Digest"
	case "ntlm":
		return "NTLM"
	case "negotiate":
		return "Negotiate"
	case "bearer":
		return "Bearer"
	}
	return scheme
}
