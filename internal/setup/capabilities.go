package setup

import "github.com/Don-Works/brw/internal/profilepolicy"

// Resolved transport names, as brw_identity and the daemon report them.
const (
	ResolvedExtensionBridge = "extension-bridge"
	ResolvedDirectCDP       = "direct-cdp"
)

// Capabilities states, for one transport, what a caller can and cannot do. The
// two lanes differ in ways that read as missing features when nothing names the
// lane: brw_open_incognito and brw_cookies exist, but only on direct CDP.
type Capabilities struct {
	Transport string `json:"transport"`
	Has       string `json:"has"`
	Lacks     string `json:"lacks"`
}

// CapabilitiesFor describes one resolved transport.
func CapabilitiesFor(transport string) Capabilities {
	if transport == ResolvedDirectCDP {
		return Capabilities{
			Transport: ResolvedDirectCDP,
			Has:       "incognito contexts (brw_open_incognito/brw_close_context), HttpOnly cookie access (brw_cookies), deterministic download routing, headless runs",
			Lacks:     "drives a separate brw-owned browser instance, not your existing signed-in window; no Chrome tab groups",
		}
	}
	return Capabilities{
		Transport: ResolvedExtensionBridge,
		Has:       "your real signed-in browser profile, its existing logins, and Chrome tab groups",
		Lacks:     "no incognito contexts (brw_open_incognito), no HttpOnly cookie access (brw_cookies), no deterministic download routing",
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
