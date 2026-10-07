package setup

import "github.com/Don-Works/brw/internal/profilepolicy"

// Resolved transport names, as brw_identity and the daemon report them.
const (
	ResolvedExtensionBridge = "extension-bridge"
	ResolvedDirectCDP       = "direct-cdp"
	// ResolvedChromeOptIn is the Chrome 144+ lane a user turns on for themselves at chrome://inspect/#remote-debugging.
	ResolvedChromeOptIn = "chrome-opt-in-cdp"
	// ResolvedRemoteCDP is `brwd --remote <endpoint>`: a DevTools endpoint some other process opened.
	ResolvedRemoteCDP = "remote-cdp"
	// ResolvedOffHostCDP is a browser a browser.provider plugin lent brwd, on the provider's machine.
	ResolvedOffHostCDP = "off-host-cdp"
)

// Capabilities states, for one transport, what a caller can and cannot do.
type Capabilities struct {
	Transport string `json:"transport"`
	Has       string `json:"has"`
	Lacks     string `json:"lacks"`
}

var capabilityTable = map[string]Capabilities{
	ResolvedDirectCDP: {
		Transport: ResolvedDirectCDP,
		Has:       "incognito contexts (brw_open_incognito/brw_close_context), HttpOnly cookie access (brw_cookies), deterministic download routing, session snapshots (brw_state), headless runs",
		Lacks:     "drives a separate brw-owned browser instance, not your existing signed-in window; no Chrome tab groups",
	},
	ResolvedChromeOptIn: {
		Transport: ResolvedChromeOptIn,
		Has:       "your real signed-in Chrome with full browser-target CDP: incognito contexts (brw_open_incognito), HttpOnly cookie access (brw_cookies), page-environment overrides — and no extension at all",
		Lacks:     "no Chrome tab groups (an extension API), no session snapshots (brw_state is refused on a browser you are signed into), no download routing (brw_set_download_path would move the downloads you make by hand, so downloads are reported without a file path), and nothing works while the chrome://inspect opt-in is off",
	},
	ResolvedRemoteCDP: {
		Transport: ResolvedRemoteCDP,
		Has:       "full browser-target CDP against a browser somebody else started: incognito contexts (brw_open_incognito), HttpOnly cookie access (brw_cookies), page-environment overrides, session snapshots (brw_state)",
		Lacks:     "no Chrome tab groups (an extension API), no download routing (brw did not start this browser, so brw_set_download_path would move files it does not own and downloads are reported without a file path), and no control over how that browser was launched — its proxy, certificate policy and headlessness were fixed by whoever started it",
	},
	ResolvedOffHostCDP: {
		Transport: ResolvedOffHostCDP,
		Has:       "full browser-target CDP against a browser on the provider's machine: incognito contexts (brw_open_incognito), HttpOnly cookie access (brw_cookies), page-environment overrides, session snapshots (brw_state)",
		Lacks:     "no Chrome tab groups (an extension API), and nothing that names something on this machine: no downloads or download routing, no file uploads (brw_upload_file resolves the path on the provider's disk) and no clipboard (brw_clipboard would read the provider host's). There is no profile on it either, so a recipe that requires the signed-in profile is refused rather than run signed out",
	},
	ResolvedExtensionBridge: {
		Transport: ResolvedExtensionBridge,
		Has:       "your real signed-in browser profile, its existing logins, and Chrome tab groups",
		Lacks:     "no incognito contexts (brw_open_incognito), no HttpOnly cookie access (brw_cookies), no deterministic download routing, no session snapshots (brw_state)",
	},
}

// CapabilitiesFor describes one resolved transport.
func CapabilitiesFor(transport string) Capabilities {
	if caps, ok := capabilityTable[transport]; ok {
		return caps
	}
	return Capabilities{
		Transport: transport,
		Has:       "unknown: brw has no capability description for this transport",
		Lacks:     "unknown: treat every capability as unverified until this lane is described",
	}
}

// ResolvedTransport reports which lane a profile actually runs on.
func ResolvedTransport(profile profilepolicy.Profile) string {
	if profile.DirectCDPAllowed {
		return ResolvedDirectCDP
	}
	if profile.ExtensionBridgeAllowed {
		return ResolvedExtensionBridge
	}
	return ""
}
