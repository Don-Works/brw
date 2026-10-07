package browser

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/plugin"
)

// RemoteTarget is a browser brw did not launch and does not own: a plugin holding browser.provider handed over a CDP websocket URL and brw drives what is on the other end of it.
type RemoteTarget struct {
	// WebSocketURL is the exact ws:// or wss:// endpoint to dial.
	WebSocketURL string
	// RedactedURL is what may be logged or put in an error.
	RedactedURL string
	// ProviderID names the plugin that minted the session.
	ProviderID string
	// SessionID is the provider's own handle for it.
	SessionID string
	// ExpiresAt is when the provider says the browser stops existing.
	ExpiresAt time.Time
	// Release gives the browser back.
	Release func(context.Context) error
}

const remoteReleaseTimeout = 30 * time.Second

// ErrRemoteTargetUnsupported is the single sentinel for every capability brw has on a browser running beside it and does not have on one running somewhere else - a plugin-supplied browser, or a --remote endpoint on another host.
var ErrRemoteTargetUnsupported = errors.New("unavailable on a browser that is not on this machine")

// ErrRemoteSessionExpired is returned once the provider's stated lifetime has passed.
var ErrRemoteSessionExpired = errors.New("the plugin-supplied browser session has expired")

// RemoteUnavailable is the closed table of what a remote target cannot do, and why.
var RemoteUnavailable = map[string]string{
	"local_screenshot_files": "screenshot destinations name files on this host; an off-host browser has no access to this account or its home-directory policy. Use a browser on this machine to save presentation files",
	"profile_reuse":          "a profile lives on the machine running the browser, and a browser on another machine has none of this machine's profiles, so --user-data-dir, --profile-directory and a workspace profile policy cannot be honoured",
	"extension_bridge": "the extension bridge drives the Chrome you are personally signed into on this machine; there is no such Chrome at the other end of a websocket to another machine. " +
		"The print-renderer screenshot fallback goes with it: it is a bridge-only path that shells out to a local PDF rasteriser",
	"profile_session": "a recipe that declares it needs the signed-in profile needs a browser a human already signed into on this machine; a browser somewhere else carries a session brw did not create and cannot attest to, so running it anyway would run a login-shaped flow signed out",
	"local_downloads": "Chrome writes a download on the machine it runs on. On a browser somewhere else the bytes land on that machine's disk, and brw's download bookkeeping would report a path that does not exist here",
	"local_upload":    "an upload hands Chrome a filesystem path, which Chrome resolves on the machine it runs on. A local path sent to a browser somewhere else names a file on that machine's disk, not the one you meant",
	"local_clipboard": "the clipboard belongs to the machine the browser runs on, so a read would return that host's clipboard and a write would set it",
	"local_session_state": "the session-snapshot store holds sessions a human signed into on THIS machine, sealed from a browser brw owns. Restoring one into a browser on another machine would put those cookies on somebody else's host — the same thing the profile gates refuse — and listing or deleting one would let a cloud-backed daemon enumerate and destroy this machine's snapshots. " +
		"Save is refused with them: a snapshot sealed from a provider's browser would join this host's store under an id indistinguishable from a local one",
}

// RemoteCapabilityNames lists the table's keys in order, for error text and for the test that enumerates them.
func RemoteCapabilityNames() []string {
	return slices.Sorted(maps.Keys(RemoteUnavailable))
}

// RemoteUnavailableError builds the named refusal for one capability.
func RemoteUnavailableError(capability string) error {
	why, ok := RemoteUnavailable[capability]
	if !ok {
		panic(fmt.Sprintf("browser: %q is not a declared remote capability; add it to RemoteUnavailable with the reason", capability))
	}
	return fmt.Errorf("%s is %w: %s", strings.ReplaceAll(capability, "_", " "), ErrRemoteTargetUnsupported, why)
}

func checkRemoteConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Remote.WebSocketURL) == "" {
		return errors.New("remote browser target has no websocket URL")
	}
	if strings.TrimSpace(cfg.Remote.RedactedURL) == "" {

		cfg.Remote.RedactedURL = RedactWebSocketURL(cfg.Remote.WebSocketURL)
	}
	return ProviderConfigProblems(cfg)
}

// ProviderConfigProblems reports every setting in a Config that conflicts with driving a browser a plugin lent brw: a second way of naming a browser, and the settings that only mean something for one brw launches itself here.
func ProviderConfigProblems(cfg Config) error {
	var problems []error
	if strings.TrimSpace(cfg.RemoteURL) != "" {
		problems = append(problems, errors.New("--remote names a CDP endpoint and a browser.provider plugin supplies one; brw will not guess which browser you meant"))
	}
	if strings.TrimSpace(cfg.BrowserWSURL) != "" {
		problems = append(problems, errors.New("a resolved browser websocket URL names a CDP endpoint and a browser.provider plugin supplies one; brw will not guess which browser you meant"))
	}
	if cfg.SignedInProfile {

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

// RedactWebSocketURL removes credentials, fragments and provider tokens from a diagnostic endpoint.
func RedactWebSocketURL(raw string) string {
	endpoint, err := plugin.ParseEndpoint(raw)
	if err != nil {
		return "the remote browser endpoint"
	}
	return endpoint.String()
}

func (m *Manager) scrubRemoteEndpoint(err error) error {
	if err == nil || m == nil || m.remote == nil {
		return err
	}
	raw := m.remote.WebSocketURL
	if raw == "" || !strings.Contains(err.Error(), raw) {
		return err
	}

	return errors.New(strings.ReplaceAll(err.Error(), raw, m.remote.RedactedURL))
}

// ProfileSessionController is a transport capability: reporting whether the browser behind it could be one a human already signed into.
type ProfileSessionController interface {
	CheckProfileSession() error
}

// CheckProfileSession reports whether this browser can carry the signed-in installed profile.
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

// BrowserOnThisHost reports whether the browser this manager drives is on the machine brwd runs on.
func (m *Manager) BrowserOnThisHost() bool {
	if m == nil {
		return true
	}
	return m.remote == nil && !m.offHost
}

// RemoteSessionInfo is the reportable description of a plugin-supplied browser session.
type RemoteSessionInfo struct {
	ProviderID string    `json:"provider_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Endpoint   string    `json:"endpoint,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
}

// RemoteSessionReporter is an optional transport capability: saying which plugin-supplied browser session this daemon is driving.
type RemoteSessionReporter interface {
	RemoteSession() (RemoteSessionInfo, bool)
}

// RemoteSession describes the provider session, for the daemon's own logging, identity and /health.
func (m *Manager) RemoteSession() (RemoteSessionInfo, bool) {
	if !m.Remote() {
		return RemoteSessionInfo{}, false
	}
	return RemoteSessionInfo{
		ProviderID: m.remote.ProviderID,
		SessionID:  m.remote.SessionID,
		Endpoint:   m.remote.RedactedURL,
		ExpiresAt:  m.remote.ExpiresAt,
	}, true
}

var _ RemoteSessionReporter = (*Manager)(nil)

func (m *Manager) checkRemoteSession() error {
	if m == nil || m.remote == nil || m.remote.ExpiresAt.IsZero() {
		return nil
	}
	if time.Now().Before(m.remote.ExpiresAt) {
		return nil
	}
	return fmt.Errorf("%w: plugin %q stated it ended at %s", ErrRemoteSessionExpired, m.remote.ProviderID, m.remote.ExpiresAt.UTC().Format(time.RFC3339))
}

func (m *Manager) refuseOnRemote(capability string) error {
	if m.BrowserOnThisHost() {
		return nil
	}
	return RemoteUnavailableError(capability)
}
