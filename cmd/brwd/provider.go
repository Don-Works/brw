package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/plugin"
)

// providerLaunch is the launch as configured, named rather than passed as a run
// of positional bools. Two adjacent bool arguments are a silent reversal
// waiting to happen, and a reversal here would refuse the wrong flag.
type providerLaunch struct {
	Bridge       bool
	UpstreamHTTP string
	ChromeOptIn  bool
	Login        bool
	Headless     bool
	Profile      string
	Workspace    string
	Config       browser.Config
}

// refuseWithProvider stops a launch that asks for a plugin-supplied browser AND
// for something only a browser on this machine can do.
//
// Every one of these is expressible together, and each would otherwise resolve
// the same way: the flag is ignored and the run goes ahead on a fresh,
// unauthenticated cloud browser. That is the exact silent downgrade a cloud
// backend must not have, so each is a named startup failure that says what was
// asked for and why this daemon cannot give it.
//
// The returned error is what the caller reports; it returns rather than exits
// so the whole table is testable.
func refuseWithProvider(launch providerLaunch) error {
	switch {
	case launch.Bridge:
		return fmt.Errorf("--bridge with a browser.provider plugin: %w", browser.RemoteUnavailableError("extension_bridge"))
	case strings.TrimSpace(launch.UpstreamHTTP) != "":
		return errors.New("--upstream-http with a browser.provider plugin: this process proxies to a daemon that already has a browser, so a provider here would mint a second one nothing drives; install the plugin on the browser-host daemon")
	case strings.TrimSpace(launch.Config.RemoteURL) != "":
		return errors.New("--remote with a browser.provider plugin: both name a browser to attach to, and brw will not guess which one you meant")
	case launch.ChromeOptIn:
		// Not folded into the --remote case even though the opt-in resolves to
		// an endpoint: the thing being refused is that --chrome-opt-in names
		// the Chrome this human is signed into on this machine, which is the
		// opposite of what a provider mints.
		return fmt.Errorf("--chrome-opt-in with a browser.provider plugin: %w", browser.RemoteUnavailableError("profile_session"))
	case launch.Login:
		return fmt.Errorf("--login with a browser.provider plugin: %w", browser.RemoteUnavailableError("profile_reuse"))
	case launch.Profile != "" || launch.Workspace != "":
		return fmt.Errorf("--profile/--workspace with a browser.provider plugin: %w", browser.RemoteUnavailableError("profile_reuse"))
	case launch.Headless:
		return errors.New("--headless with a browser.provider plugin: the provider launched its own browser, so whether it has a window was decided over there")
	case len(launch.Config.Extensions) > 0:
		return errors.New("--extension with a browser.provider plugin: an unpacked extension is loaded from this machine's filesystem, which the provider's browser cannot read")
	case len(launch.Config.ChromeArgs) > 0:
		return errors.New("--chrome-arg with a browser.provider plugin: brw is not launching Chrome, so it has no command line to add to")
	case launch.Config.Port != 0:
		// checkRemoteConfig refuses this too, but only inside browser.New —
		// which runs after the operator's mint program has already executed and
		// the provider has already billed a session. A conflict that is knowable
		// from the flags belongs in the startup table, before anything is minted.
		return errors.New("--remote-debugging-port with a browser.provider plugin: brw is not launching Chrome, so there is no debugging port for it to open")
	case launch.Config.AllowRealProfile:
		return fmt.Errorf("--unsafe-real-profile with a browser.provider plugin: %w", browser.RemoteUnavailableError("profile_reuse"))
	case !launch.Config.Network.Empty():
		return errors.New("--proxy-server/--ignore-https-errors/--ca-cert with a browser.provider plugin: they are Chrome launch switches, and the provider launched its own browser")
	}
	return nil
}

// explicitProfileFlags reports an operator who NAMED a profile directory, as
// opposed to the flag default. The default describes this machine and is
// cleared; refusing on it would make a provider unusable without also passing a
// flag to unset one.
func explicitProfileFlags() bool {
	return flagWasSet("user-data-dir") || os.Getenv("BRW_USER_DATA_DIR") != "" ||
		flagWasSet("profile-directory") || os.Getenv("BRW_PROFILE_DIRECTORY") != ""
}

// providerReleaseTimeout bounds giving a browser back on the failure path. The
// caller's context is usually already cancelled by the time anything releases,
// so the teardown gets a deadline of its own.
const providerReleaseTimeout = 30 * time.Second

// releaseUnreleasedSession gives a plugin-supplied browser back after a failed
// start, and only when nothing already has.
//
// browser.New calls Manager.Close when the connect fails, which releases the
// session and clears Release on this very pointer. Running the operator's
// teardown program a second time resolves the provider credential again and logs
// a second exit status as a failed release, which reads as a leaked session when
// nothing leaked. A failure BEFORE the connect — checkRemoteConfig refusing the
// configuration — leaves Release set, and that is the case where the provider is
// still holding a browser nothing will ever connect to.
func releaseUnreleasedSession(remote *browser.RemoteTarget, release func(context.Context) error) {
	if remote == nil || remote.Release == nil || release == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), providerReleaseTimeout)
	defer cancel()
	if err := release(releaseCtx); err != nil {
		log.Printf("release the plugin-supplied browser session: %v", err)
	}
}

// openProviderBrowser mints a session and turns it into the remote target the
// browser manager drives.
//
// The redacted endpoint is the only form that reaches a log line. The full
// websocket URL authenticates the connection — Chrome's own /devtools/browser/
// path is a bearer token and a hosted provider usually carries its key in the
// query — so it goes to the CDP dialer and nowhere else. It returns rather than
// exiting on failure so a test can call it and read what it logged.
func openProviderBrowser(ctx context.Context, plugins *plugin.Registry) (*browser.RemoteTarget, func(context.Context) error, error) {
	session, release, err := plugins.OpenBrowserSession(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open a browser session from the browser.provider plugin: %w", err)
	}
	expiresAt := time.Now().Add(session.Lifetime)
	log.Printf("driving a plugin-supplied browser: provider %s, session %s, endpoint %s, session ends %s",
		session.ProviderID, session.SessionID, session.Endpoint, expiresAt.UTC().Format(time.RFC3339))
	if session.Endpoint.PlaintextToAnotherHost() {
		log.Printf("WARNING: plugin %s minted a plaintext ws:// endpoint at %s; the whole CDP session travels unencrypted to that host, including page content, cookies and anything brw types",
			session.ProviderID, session.Endpoint)
	}
	return &browser.RemoteTarget{
		WebSocketURL: session.Endpoint.Reveal(),
		RedactedURL:  session.Endpoint.String(),
		ProviderID:   session.ProviderID,
		SessionID:    session.SessionID,
		ExpiresAt:    expiresAt,
		Release:      release,
	}, release, nil
}
