package browser

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RemoteTarget is a browser brw did not launch and does not own: a plugin
// holding browser.provider handed over a CDP websocket URL and brw drives what
// is on the other end of it.
//
// A remote target is LESS trusted than a local one, not more. Everything above
// the socket is unchanged — the navigation policy, the containment boundary,
// the site-consent gate, the identity guard — and the things that only work
// because the browser is on this machine are refused by name rather than
// attempted and silently getting the wrong answer.
type RemoteTarget struct {
	// WebSocketURL is the exact ws:// or wss:// endpoint to dial. brw hands it
	// to the CDP dialer as given: rewriting it (resolving the host to an IP, or
	// fetching /json/version first) is brw second-guessing the provider, and on
	// a wss endpoint it breaks the TLS handshake.
	WebSocketURL string
	// RedactedURL is what may be logged or put in an error. The path of a CDP
	// websocket URL authenticates the connection, so the full URL is never one
	// or the other.
	RedactedURL string
	// ProviderID names the plugin that minted the session.
	ProviderID string
	// SessionID is the provider's own handle for it.
	SessionID string
	// ExpiresAt is when the provider says the browser stops existing. brw
	// refuses to START an operation past it, so an expired session is a named
	// error rather than a socket failure nobody can attribute.
	ExpiresAt time.Time
	// Release gives the browser back. Called once, from Manager.Close.
	Release func(context.Context) error
}

// remoteReleaseTimeout bounds giving the browser back. Close runs on the way
// out of a daemon, often with every caller context already cancelled, so the
// release gets a deadline of its own rather than inheriting one that has
// already fired.
const remoteReleaseTimeout = 30 * time.Second

// ErrRemoteTargetUnsupported is the single sentinel for every capability brw
// has on a browser it launched and does not have on one a plugin lent it. A
// caller branches on the class with errors.Is rather than matching strings.
var ErrRemoteTargetUnsupported = errors.New("unavailable on a plugin-supplied remote browser")

// ErrRemoteSessionExpired is returned once the provider's stated lifetime has
// passed. Starting an operation on a browser the provider has reclaimed
// produces a websocket error with no attribution; this names it.
var ErrRemoteSessionExpired = errors.New("the plugin-supplied browser session has expired")

// RemoteUnavailable is the closed table of what a remote target cannot do, and
// why. It is a table, not a set of scattered checks, because a gate spread
// across a dozen call sites is a gate with a hole in it — and because the
// sibling verb is what gets missed: refusing brw_downloads and leaving
// brw_set_download_path pointing Chrome at a directory on somebody else's
// machine is worse than refusing neither.
//
// Every value here is a REASON, written for whoever reads the error. A reason
// that only says "not supported" tells an agent nothing about what to do next.
var RemoteUnavailable = map[string]string{
	"profile_reuse": "a profile lives on the machine running the browser; a plugin-supplied browser has none of this machine's profiles, so --user-data-dir, --profile-directory and a workspace profile policy cannot be honoured",
	"extension_bridge": "the extension bridge drives the Chrome you are personally signed into on this machine; there is no such Chrome at the other end of a provider's websocket. " +
		"The print-renderer screenshot fallback goes with it: it is a bridge-only path that shells out to a local PDF rasteriser",
	"profile_session": "a recipe that declares it needs the signed-in profile needs a browser a human already signed into; a provider mints a fresh unauthenticated one, so running it anyway would run a login-shaped flow signed out",
	"local_downloads": "Chrome writes a download on the machine it runs on. On a provider's browser the bytes land on the provider's disk, and brw's download bookkeeping would report a path that does not exist here",
	"local_upload":    "an upload hands Chrome a filesystem path, which Chrome resolves on the machine it runs on. A local path sent to a provider's browser names a file on the provider's disk, not the one you meant",
	"local_clipboard": "the clipboard belongs to the machine the browser runs on, so a read would return the provider host's clipboard and a write would set it",
}

// RemoteCapabilityNames lists the table's keys in order, for error text and for
// the test that enumerates them.
func RemoteCapabilityNames() []string {
	names := make([]string, 0, len(RemoteUnavailable))
	for name := range RemoteUnavailable {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RemoteUnavailableError builds the named refusal for one capability. It panics
// on an unknown name rather than inventing a message: an unnamed refusal is the
// silent degrade this whole file exists to prevent, and a typo here would ship
// as one.
func RemoteUnavailableError(capability string) error {
	why, ok := RemoteUnavailable[capability]
	if !ok {
		panic(fmt.Sprintf("browser: %q is not a declared remote capability; add it to RemoteUnavailable with the reason", capability))
	}
	return fmt.Errorf("%s is %w: %s", strings.ReplaceAll(capability, "_", " "), ErrRemoteTargetUnsupported, why)
}

// checkRemoteConfig refuses a configuration that mixes a plugin-supplied
// browser with the settings that only mean something for one brw launches.
//
// Refused, not ignored. Every field here describes the machine the browser runs
// on, and quietly dropping it is how an operator ends up believing a cloud run
// reused their signed-in profile, went through their proxy, or trusted their
// private CA. The error names the field and the reason.
func checkRemoteConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Remote.WebSocketURL) == "" {
		return errors.New("remote browser target has no websocket URL")
	}
	if strings.TrimSpace(cfg.Remote.RedactedURL) == "" {
		// A caller that forgot it would otherwise have every scrubbed error
		// replace the URL with nothing, which reads as a message about no host
		// at all. Derived rather than refused: the scrub has to work whether or
		// not whoever built the target remembered.
		cfg.Remote.RedactedURL = redactWebSocketURL(cfg.Remote.WebSocketURL)
	}
	var problems []error
	if strings.TrimSpace(cfg.RemoteURL) != "" {
		problems = append(problems, errors.New("--remote names a CDP endpoint and a browser.provider plugin supplies one; brw will not guess which browser you meant"))
	}
	if strings.TrimSpace(cfg.BrowserWSURL) != "" {
		problems = append(problems, errors.New("a resolved browser websocket URL names a CDP endpoint and a browser.provider plugin supplies one; brw will not guess which browser you meant"))
	}
	if cfg.SignedInProfile {
		// The one field here that is a CLAIM rather than a setting, and the
		// dangerous direction is the one that would be silently accepted: a
		// provider's browser marked as the one its user is signed into would
		// pass brw_state's refusal and seal a freshly minted anonymous session
		// as if it were that person's.
		problems = append(problems, RemoteUnavailableError("profile_session"))
	}
	if strings.TrimSpace(cfg.UserDataDir) != "" || strings.TrimSpace(cfg.ProfileDirectory) != "" {
		problems = append(problems, RemoteUnavailableError("profile_reuse"))
	}
	if len(cfg.Extensions) > 0 {
		problems = append(problems, errors.New("--extension loads an unpacked extension from this machine's filesystem, which the provider's browser cannot read"))
	}
	if len(cfg.ChromeArgs) > 0 || cfg.Headless || cfg.AllowRealProfile || cfg.Port != 0 {
		problems = append(problems, errors.New("chrome launch settings (--chrome-arg, --headless, --remote-debugging-port, --unsafe-real-profile) are decided by whoever started the provider's browser, not by brw"))
	}
	if !cfg.Network.Empty() {
		problems = append(problems, errors.New("--proxy-server, --ignore-https-errors and --ca-cert are Chrome launch switches; the provider launched its own browser, so brw cannot apply them"))
	}
	return errors.Join(problems...)
}

// redactWebSocketURL keeps scheme://host and drops everything that
// authenticates. Used only as the fallback when a caller built a RemoteTarget
// without one; internal/plugin.Endpoint is the authoritative redactor, and it
// is what a provider-minted session carries.
func redactWebSocketURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	scheme, rest, found := strings.Cut(trimmed, "://")
	if !found {
		return "the remote browser endpoint"
	}
	host, _, _ := strings.Cut(rest, "/")
	host, _, _ = strings.Cut(host, "?")
	if host == "" {
		return "the remote browser endpoint"
	}
	return scheme + "://" + host
}

// scrubRemoteEndpoint keeps the session URL out of an error. The CDP dialer
// quotes the URL it could not reach, and on a remote target the path of that
// URL is what authenticates the connection.
//
// A substring replace is a net, not a proof: a dialer that percent-encoded the
// URL, or split it across two sentences, writes a form this walks past. It is
// the last line rather than the boundary — the boundary is that only the dialer
// is ever given the whole URL.
func (m *Manager) scrubRemoteEndpoint(err error) error {
	if err == nil || m == nil || m.remote == nil {
		return err
	}
	raw := m.remote.WebSocketURL
	if raw == "" || !strings.Contains(err.Error(), raw) {
		return err
	}
	// Deliberately not wrapped: the original's own Error() still holds the URL,
	// so a wrapped chain would keep a live copy for anything walking Unwrap.
	return errors.New(strings.ReplaceAll(err.Error(), raw, m.remote.RedactedURL))
}

// ProfileSessionController is a transport capability: reporting whether the
// browser behind it could be one a human already signed into.
//
// It answers a question no other surface can. A recipe that declares it needs
// the installed profile is declaring that its steps assume an authenticated
// session; run it on a freshly minted cloud browser and every step still
// "works" until the flow lands on a login page, by which point it has already
// clicked through half a site as an anonymous visitor.
//
// Both first-party transports implement it, so their answer is a named error
// rather than a missing method that a type assertion quietly treats as
// "probably fine". The upstream HTTP controller deliberately does not: a proxy
// would be answering for a transport it only forwards to, and the recipe runner
// that asks the question lives on the browser host anyway. A caller that finds
// no implementation refuses rather than assuming — see
// recipe.Runner.checkRequirements.
type ProfileSessionController interface {
	CheckProfileSession() error
}

// CheckProfileSession reports whether this browser can carry the signed-in
// installed profile. A brw-launched (or --remote local) Chrome can: its profile
// directory is on this machine and a human may have signed into it. A
// provider's browser cannot, whatever the provider's marketing says about
// persistent contexts — brw did not create that state and cannot attest to it.
func (m *Manager) CheckProfileSession() error {
	return m.refuseOnRemote("profile_session")
}

var _ ProfileSessionController = (*Manager)(nil)

// Remote reports whether this manager drives a plugin-supplied browser.
func (m *Manager) Remote() bool {
	if m == nil {
		return false
	}
	return m.remote != nil
}

// RemoteSession describes the provider session, for the daemon's own logging
// and identity. It never carries the unredacted endpoint.
func (m *Manager) RemoteSession() (providerID, sessionID, redactedURL string, expiresAt time.Time, ok bool) {
	if !m.Remote() {
		return "", "", "", time.Time{}, false
	}
	return m.remote.ProviderID, m.remote.SessionID, m.remote.RedactedURL, m.remote.ExpiresAt, true
}

// checkRemoteSession refuses to start work on a session the provider has
// already reclaimed. Called from the two funnels every browser operation passes
// through (tabContext for anything tab-scoped, runBrowser for anything
// browser-scoped), so a new verb inherits the check instead of having to
// remember it.
func (m *Manager) checkRemoteSession() error {
	if m == nil || m.remote == nil || m.remote.ExpiresAt.IsZero() {
		return nil
	}
	if time.Now().Before(m.remote.ExpiresAt) {
		return nil
	}
	return fmt.Errorf("%w: plugin %q stated it ended at %s", ErrRemoteSessionExpired, m.remote.ProviderID, m.remote.ExpiresAt.UTC().Format(time.RFC3339))
}

// refuseOnRemote is the one-line guard the refused verbs open with. It returns
// nil on a local browser, so the check costs a nil comparison there.
func (m *Manager) refuseOnRemote(capability string) error {
	if !m.Remote() {
		return nil
	}
	return RemoteUnavailableError(capability)
}
