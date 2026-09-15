package mcp

import (
	"sort"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// toolRequirement is what a capability-gated tool needs from a transport, or
// what about a transport forbids it. Tools are classified by requirement rather
// than by listing the transports that lack them, because the list form has to
// be revisited by hand for every new lane and the lane that gets forgotten is
// the one where a tool is advertised and then always fails. brw shipped two
// lanes for long enough that "the other transport" was a workable shorthand;
// the Chrome opt-in lane is the third, and it is direct-CDP-capable against a
// browser the user is signed into, so it matches neither of the old two.
type toolRequirement int

const (
	// needsCDPSession: DevTools Protocol session state that has to survive
	// between calls — an override installed once and read by every later
	// navigation, a held modifier stamped onto later input events. A lane that
	// attaches and detaches around each operation drops it.
	needsCDPSession toolRequirement = iota
	// needsBrowserTarget: the CDP browser target rather than a page. Incognito
	// contexts, browser-level cookie access and permission grants live there,
	// and an extension cannot attach chrome.debugger to that target at all.
	needsBrowserTarget
	// needsExtensionAPIs: a chrome.* API with no DevTools Protocol equivalent.
	// Chrome tab groups are the whole of this class.
	needsExtensionAPIs
	// refusedOnSignedInProfile: capable everywhere, refused where the browser is
	// the one the user is personally signed into. Policy, not a gap; see
	// docs/auth-model.md.
	refusedOnSignedInProfile
	// needsDownloadRouting: the lane can be told at runtime where a completed
	// download lands. A durable CDP session is not enough — the CDP command is
	// browser-context-wide, so a lane whose browser context belongs to the user
	// cannot use it without moving the files that person downloads by hand.
	needsDownloadRouting
)

// toolRequirements classifies every tool whose availability depends on the
// transport. A tool absent from this map is available on every lane.
//
// Each entry mirrors a controller method that returns an Err*Unsupported
// sentinel on the lanes this derives: the map decides what tools/list
// advertises, and the controller is what actually refuses, so the two must
// agree or an agent is told about a capability that errors.
var toolRequirements = map[string]toolRequirement{
	"brw_open_incognito": needsBrowserTarget,
	"brw_close_context":  needsBrowserTarget,
	// Cookie access is browser-level (Storage.getCookies against a browser
	// context), and the extension's own security policy blocks the CDP cookie
	// methods outright to protect the signed-in profile.
	"brw_cookies": needsBrowserTarget,
	// Clipboard access needs the browser-level Browser.setPermission command.
	"brw_clipboard": needsBrowserTarget,

	"brw_group_tabs":      needsExtensionAPIs,
	"brw_ungroup_tabs":    needsExtensionAPIs,
	"brw_list_tab_groups": needsExtensionAPIs,

	// Page-environment overrides are DevTools Protocol session state; a detach
	// drops every one the session installed.
	"brw_set_geolocation":        needsCDPSession,
	"brw_set_network_conditions": needsCDPSession,
	"brw_emulate_media":          needsCDPSession,
	"brw_set_extra_headers":      needsCDPSession,
	"brw_set_user_agent":         needsCDPSession,
	"brw_authenticate":           needsCDPSession,
	// Not needsCDPSession: the Chrome opt-in lane has the session and still
	// cannot route downloads. See needsDownloadRouting.
	"brw_set_download_path": needsDownloadRouting,
	// Held keys need the transport to stamp a modifier mask onto every later
	// input event, and a policy-checked same-document history change needs the
	// controller to resolve the target against the live document across calls.
	"brw_key_down":  needsCDPSession,
	"brw_key_up":    needsCDPSession,
	"brw_pushstate": needsCDPSession,

	"brw_state": refusedOnSignedInProfile,
}

// runnableOn reports whether a transport satisfies a requirement. It reads the
// transport's declared properties rather than a per-tool transport list, so
// adding a lane means declaring what that lane can do once.
func runnableOn(req toolRequirement, caps brwidentity.TransportCapabilities) bool {
	switch req {
	case needsCDPSession:
		return caps.CDPSession
	case needsBrowserTarget:
		return caps.BrowserTarget
	case needsExtensionAPIs:
		return caps.ExtensionAPIs
	case refusedOnSignedInProfile:
		return !caps.SignedInProfile
	case needsDownloadRouting:
		return caps.RuntimeDownloadRouting
	default:
		// An unclassified requirement must not read as "available everywhere".
		// A test enumerates the requirements so this branch stays unreachable.
		return false
	}
}

// transportUnsupported is the derived table: tool -> the transports on which it
// can never succeed. Built once at init from toolRequirements and the transport
// properties, so the two can never disagree.
var transportUnsupported = buildTransportUnsupported()

func buildTransportUnsupported() map[string][]string {
	out := make(map[string][]string, len(toolRequirements))
	for tool, req := range toolRequirements {
		var bad []string
		for _, transport := range brwidentity.Transports() {
			caps, known := brwidentity.Capabilities(transport)
			if !known || !runnableOn(req, caps) {
				bad = append(bad, transport)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			out[tool] = bad
		}
	}
	return out
}

// unsupportedOn reports whether a tool can never succeed on a transport.
func unsupportedOn(tool, transport string) bool {
	for _, bad := range transportUnsupported[tool] {
		if bad == transport {
			return true
		}
	}
	return false
}
