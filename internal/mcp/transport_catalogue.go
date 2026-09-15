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
	// needsLocalBrowserHost: the verb names something that exists on a machine —
	// a filesystem path, the clipboard — and is only the thing the caller meant
	// when the browser is on the machine brwd runs on. This is the one class
	// that does not fail when it is missing: the CDP command succeeds against
	// the provider's host and answers about the wrong disk.
	needsLocalBrowserHost
)

// requirementNames is what a requirement is called in a test failure, and the
// enumeration a test walks so a constant added above without a rule in
// runnableOn cannot reach the default branch unnoticed.
var requirementNames = map[toolRequirement]string{
	needsCDPSession:          "needsCDPSession",
	needsBrowserTarget:       "needsBrowserTarget",
	needsExtensionAPIs:       "needsExtensionAPIs",
	refusedOnSignedInProfile: "refusedOnSignedInProfile",
	needsDownloadRouting:     "needsDownloadRouting",
	needsLocalBrowserHost:    "needsLocalBrowserHost",
}

// toolRequirements classifies every tool whose availability depends on the
// transport. A tool absent from this map is available on every lane.
//
// Each entry mirrors a controller method that returns an Err*Unsupported
// sentinel on the lanes this derives: the map decides what tools/list
// advertises, and the controller is what actually refuses, so the two must
// agree or an agent is told about a capability that errors.
//
// The value is a LIST because a tool can be impossible for more than one
// unrelated reason, and a single requirement makes the second unrepresentable:
// brw_clipboard needs the browser target the extension bridge cannot attach to,
// AND it needs the clipboard to be this machine's. Those exclude different
// lanes, and a row that could only state one of them would advertise the tool
// on the lane the other one covers. A tool is unsupported where ANY of its
// requirements is unmet.
var toolRequirements = map[string][]toolRequirement{
	"brw_open_incognito": {needsBrowserTarget},
	"brw_close_context":  {needsBrowserTarget},
	// Cookie access is browser-level (Storage.getCookies against a browser
	// context), and the extension's own security policy blocks the CDP cookie
	// methods outright to protect the signed-in profile.
	"brw_cookies": {needsBrowserTarget},
	// Clipboard access needs the browser-level Browser.setPermission command,
	// which the bridge's per-tab chrome.debugger session cannot send. Off host
	// the command works and answers about the WRONG machine: the clipboard
	// belongs to the host the browser runs on, so a read returns that host's
	// and a write sets it.
	"brw_clipboard": {needsBrowserTarget, needsLocalBrowserHost},

	"brw_group_tabs":      {needsExtensionAPIs},
	"brw_ungroup_tabs":    {needsExtensionAPIs},
	"brw_list_tab_groups": {needsExtensionAPIs},

	// Page-environment overrides are DevTools Protocol session state; a detach
	// drops every one the session installed.
	"brw_set_geolocation":        {needsCDPSession},
	"brw_set_network_conditions": {needsCDPSession},
	"brw_emulate_media":          {needsCDPSession},
	"brw_set_extra_headers":      {needsCDPSession},
	"brw_set_user_agent":         {needsCDPSession},
	"brw_authenticate":           {needsCDPSession},
	// Not needsCDPSession: the Chrome opt-in lane has the session and still
	// cannot route downloads. See needsDownloadRouting. The second requirement
	// is a different refusal, not a restatement: off host the routing command
	// succeeds and creates the directory on the provider's disk.
	"brw_set_download_path": {needsDownloadRouting, needsLocalBrowserHost},
	// Downloads and uploads are the same fact twice: Chrome resolves a
	// filesystem path on the machine it runs on. Both verbs, because refusing
	// the reader and leaving the writer is how a gate ends up covering half a
	// pair — brw_downloads would report paths that do not exist here, and
	// brw_upload_file would hand the provider's Chrome a path naming a file on
	// its disk rather than the one the caller meant.
	"brw_downloads":   {needsLocalBrowserHost},
	"brw_upload_file": {needsLocalBrowserHost},
	// Held keys need the transport to stamp a modifier mask onto every later
	// input event, and a policy-checked same-document history change needs the
	// controller to resolve the target against the live document across calls.
	"brw_key_down":  {needsCDPSession},
	"brw_key_up":    {needsCDPSession},
	"brw_pushstate": {needsCDPSession},

	"brw_state": {refusedOnSignedInProfile},
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
	case needsLocalBrowserHost:
		return caps.BrowserOnThisHost
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
	for tool, reqs := range toolRequirements {
		var bad []string
		for _, transport := range brwidentity.Transports() {
			caps, known := brwidentity.Capabilities(transport)
			if !known || !runnableOnAll(reqs, caps) {
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

// runnableOnAll reports whether a transport satisfies every requirement a tool
// carries. An empty list is satisfied, which is why a tool absent from
// toolRequirements runs everywhere.
func runnableOnAll(reqs []toolRequirement, caps brwidentity.TransportCapabilities) bool {
	for _, req := range reqs {
		if !runnableOn(req, caps) {
			return false
		}
	}
	return true
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
