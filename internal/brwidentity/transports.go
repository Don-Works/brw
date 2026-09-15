package brwidentity

import "sort"

// TransportCapabilities are the three properties that decide, for every
// capability-gated tool, whether a lane can run it. They are stated per
// transport once, here, because the alternative is a per-tool list that has to
// be revisited by hand for every new lane — and the lane that gets forgotten is
// the one where a tool is advertised and then always fails.
//
// They describe the lane, not the browser: Chrome's full DevTools Protocol is
// reachable on two of the three lanes, and the third has extension APIs CDP
// does not expose at all.
type TransportCapabilities struct {
	// CDPSession reports that brw holds a DevTools Protocol session it owns for
	// the life of the daemon. Everything that is session state — a geolocation
	// override, a held modifier, an intercepted request — needs one, and a lane
	// that attaches and detaches around each operation does not have one.
	CDPSession bool
	// BrowserTarget reports that brw can talk to the CDP BROWSER target, not
	// just a page. Incognito contexts, browser-level cookie access and
	// permission grants live there.
	BrowserTarget bool
	// ExtensionAPIs reports that brw reaches chrome.* extension APIs. Chrome
	// tab groups exist only there.
	ExtensionAPIs bool
	// SignedInProfile reports that the lane drives the browser the user is
	// personally signed into. It is not a capability: it is why brw refuses to
	// seal that browser's cookies into a snapshot (docs/auth-model.md).
	SignedInProfile bool
}

// transportCapabilities is the authoritative table. A transport absent from it
// is not a transport brw can report, which is what KnownTransport enforces.
var transportCapabilities = map[string]TransportCapabilities{
	// brw launched this browser itself, against a brw-owned profile directory.
	TransportDirectCDP: {CDPSession: true, BrowserTarget: true},
	// The user turned on remote debugging at chrome://inspect/#remote-debugging
	// in their own Chrome. brw attaches to the browser target it exposes, so
	// everything CDP offers is reachable — against the profile they are signed
	// into, which is why SignedInProfile is set.
	TransportChromeOptIn: {CDPSession: true, BrowserTarget: true, SignedInProfile: true},
	// The extension attaches chrome.debugger per operation and detaches after,
	// so there is no durable session and no browser target; what it does have is
	// the extension APIs.
	TransportExtensionBridge: {ExtensionAPIs: true, SignedInProfile: true},
}

// Transports lists every transport brw can report, sorted so callers that
// render it produce a stable order.
func Transports() []string {
	out := make([]string, 0, len(transportCapabilities))
	for name := range transportCapabilities {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Capabilities returns what a transport can do. The second result is false for
// a name that is not a transport, which is how a caller tells "this lane cannot
// do it" apart from "nobody has classified this lane".
func Capabilities(transport string) (TransportCapabilities, bool) {
	caps, ok := transportCapabilities[transport]
	return caps, ok
}

// KnownTransport reports whether name is a transport brw classifies.
func KnownTransport(transport string) bool {
	_, ok := transportCapabilities[transport]
	return ok
}
