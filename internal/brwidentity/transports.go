package brwidentity

import (
	"maps"
	"slices"
)

// TransportCapabilities are the properties that decide, for every capability-gated tool, whether a lane can run it.
type TransportCapabilities struct {
	// CDPSession reports that brw holds a DevTools Protocol session it owns for the life of the daemon.
	CDPSession bool
	// BrowserTarget reports that brw can talk to the CDP BROWSER target, not just a page.
	BrowserTarget bool
	// ExtensionAPIs reports that brw reaches chrome.* extension APIs.
	ExtensionAPIs bool
	// SignedInProfile reports that the lane drives the browser the user is personally signed into.
	SignedInProfile bool
	// RuntimeDownloadRouting reports that brw can decide, while the browser is running, where a completed download lands.
	RuntimeDownloadRouting bool
	// BrowserOnThisHost reports that the browser is running on the machine brwd is running on, so a filesystem path, the clipboard and this host's session-snapshot store mean the same thing at both ends of the socket.
	BrowserOnThisHost bool
}

var transportCapabilities = map[string]TransportCapabilities{

	TransportDirectCDP: {CDPSession: true, BrowserTarget: true, RuntimeDownloadRouting: true, BrowserOnThisHost: true},

	TransportChromeOptIn: {CDPSession: true, BrowserTarget: true, SignedInProfile: true, BrowserOnThisHost: true},

	TransportRemoteCDP: {CDPSession: true, BrowserTarget: true, BrowserOnThisHost: true},

	TransportOffHostCDP: {CDPSession: true, BrowserTarget: true},

	TransportExtensionBridge: {ExtensionAPIs: true, SignedInProfile: true, BrowserOnThisHost: true},
}

// Transports lists every transport brw can report, sorted so callers that render it produce a stable order.
func Transports() []string {
	return slices.Sorted(maps.Keys(transportCapabilities))
}

// Capabilities returns what a transport can do.
func Capabilities(transport string) (TransportCapabilities, bool) {
	caps, ok := transportCapabilities[transport]
	return caps, ok
}

// KnownTransport reports whether name is a transport brw classifies.
func KnownTransport(transport string) bool {
	_, ok := transportCapabilities[transport]
	return ok
}
