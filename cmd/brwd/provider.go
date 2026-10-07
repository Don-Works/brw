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

func refuseWithProvider(launch providerLaunch) error {
	switch {
	case launch.Bridge:
		return fmt.Errorf("--bridge with a browser.provider plugin: %w", browser.RemoteUnavailableError("extension_bridge"))
	case strings.TrimSpace(launch.UpstreamHTTP) != "":
		return errors.New("--upstream-http with a browser.provider plugin: this process proxies to a daemon that already has a browser, so a provider here would mint a second one nothing drives; install the plugin on the browser-host daemon")
	case strings.TrimSpace(launch.Config.RemoteURL) != "":
		return errors.New("--remote with a browser.provider plugin: both name a browser to attach to, and brw will not guess which one you meant")
	case launch.ChromeOptIn:

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

		return errors.New("--remote-debugging-port with a browser.provider plugin: brw is not launching Chrome, so there is no debugging port for it to open")
	case launch.Config.AllowRealProfile:
		return fmt.Errorf("--unsafe-real-profile with a browser.provider plugin: %w", browser.RemoteUnavailableError("profile_reuse"))
	case !launch.Config.Network.Empty():
		return errors.New("--proxy-server/--ignore-https-errors/--ca-cert with a browser.provider plugin: they are Chrome launch switches, and the provider launched its own browser")
	}
	return nil
}

func explicitProfileFlags() bool {
	return flagWasSet("user-data-dir") || os.Getenv("BRW_USER_DATA_DIR") != "" ||
		flagWasSet("profile-directory") || os.Getenv("BRW_PROFILE_DIRECTORY") != ""
}

const providerReleaseTimeout = 30 * time.Second

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
