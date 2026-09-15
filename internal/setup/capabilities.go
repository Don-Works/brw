package setup

import "github.com/Don-Works/brw/internal/profilepolicy"

// Resolved transport names, as brw_identity and the daemon report them. They
// are the brwidentity.Transport* values; this package keeps its own constants
// so the setup surface does not pull the identity package into every caller.
const (
	ResolvedExtensionBridge = "extension-bridge"
	ResolvedDirectCDP       = "direct-cdp"
	// ResolvedChromeOptIn is the Chrome 144+ lane a user turns on for
	// themselves at chrome://inspect/#remote-debugging. No profile policy
	// selects it — `brwd --chrome-opt-in` does, against whichever Chrome has
	// the switch on — so ResolvedTransport never returns it. It is here so
	// doctor and the capability table can describe a daemon that reports it.
	ResolvedChromeOptIn = "chrome-opt-in-cdp"
	// ResolvedRemoteCDP is `brwd --remote <endpoint>`: a DevTools endpoint some
	// other process opened. No profile policy selects it either, so
	// ResolvedTransport never returns it; doctor describes a daemon reporting
	// it from here.
	ResolvedRemoteCDP = "remote-cdp"
)

// Capabilities states, for one transport, what a caller can and cannot do. The
// lanes differ in ways that read as missing features when nothing names the
// lane: brw_open_incognito and brw_cookies exist on every lane but the
// extension bridge, Chrome tab groups only on that one, and download routing
// only where brw started the browser.
type Capabilities struct {
	Transport string `json:"transport"`
	Has       string `json:"has"`
	Lacks     string `json:"lacks"`
}

// capabilityTable is what each lane can and cannot do, in the words doctor
// shows a human. A lane absent from it is described as unclassified rather than
// falling through to another lane's text: a wrong capability list reads as an
// authoritative one, and the reader has nothing to check it against.
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

// ResolvedTransport reports which lane a profile actually runs on. A profile
// that allows both is reported as direct CDP, matching how `mcp-config --mode
// auto` picks a runtime mode.
func ResolvedTransport(profile profilepolicy.Profile) string {
	if profile.DirectCDPAllowed {
		return ResolvedDirectCDP
	}
	if profile.ExtensionBridgeAllowed {
		return ResolvedExtensionBridge
	}
	return ""
}
