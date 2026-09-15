package brwidentity

import "sort"

// TransportCapabilities are the properties that decide, for every
// capability-gated tool, whether a lane can run it. They are stated per
// transport once, here, because the alternative is a per-tool list that has to
// be revisited by hand for every new lane — and the lane that gets forgotten is
// the one where a tool is advertised and then always fails.
//
// They describe the lane, not the browser: Chrome's full DevTools Protocol is
// reachable on every lane but the extension bridge, and the bridge has
// extension APIs CDP does not expose at all.
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
	// seal that browser's cookies into a snapshot (docs/auth-model.md), and why
	// it will not retarget that browser's downloads.
	SignedInProfile bool
	// RuntimeDownloadRouting reports that brw can decide, while the browser is
	// running, where a completed download lands. It is separate from CDPSession
	// because the primitive is not one: Browser.setDownloadBehavior applies to a
	// whole browser context, so in a browser somebody else is using it would
	// redirect the downloads that person starts by hand.
	//
	// It is true on exactly the lanes where brw started the browser, which is
	// the same question internal/browser asks at runtime (Manager.stagesDownloads).
	// It is NOT the same question as SignedInProfile: `--remote` may well be
	// pointed at a throwaway browser nobody is signed into, and brw still must
	// not move its files, because whoever started that browser chose where they
	// land.
	//
	// WebDriver BiDi has no equivalent at all — browsingContext.setDownloadBehavior
	// is an unknown command on Firefox (docs/bidi-prototype.md) — so a future
	// BiDi lane would declare this false while declaring a durable session.
	RuntimeDownloadRouting bool
}

// transportCapabilities is the authoritative table. A transport absent from it
// is not a transport brw can report, which is what KnownTransport enforces.
var transportCapabilities = map[string]TransportCapabilities{
	// brw started this browser itself, so nobody else is downloading in it.
	// That, and only that, is what RuntimeDownloadRouting states.
	TransportDirectCDP: {CDPSession: true, BrowserTarget: true, RuntimeDownloadRouting: true},
	// The user turned on remote debugging at chrome://inspect/#remote-debugging
	// in their own Chrome. brw attaches to the browser target it exposes, so
	// everything CDP offers is reachable — against the profile they are signed
	// into, which is why SignedInProfile is set. RuntimeDownloadRouting is not:
	// the browser context is the user's own, and pointing it at brw's staging
	// directory would move the files they download by hand.
	TransportChromeOptIn: {CDPSession: true, BrowserTarget: true, SignedInProfile: true},
	// brw attached to a DevTools endpoint another process opened. Everything CDP
	// offers is reachable, and nothing about the browser behind it is known:
	// brw did not start it and cannot tell who else is using it.
	// RuntimeDownloadRouting is therefore false, for the same reason it is false
	// on the opt-in lane — the command is browser-context-wide and this browser
	// context is not brw's to redirect.
	//
	// SignedInProfile is NOT set. It gates brw_state, which seals cookies only
	// when an operator asks for it by name, on a browser they chose to point brw
	// at; refusing that on an endpoint that is usually a throwaway automation
	// Chrome would take away a working capability to guess at a risk nobody
	// reported. The download refusal needs no such guess because nothing asks
	// for it: a read-shaped brw_downloads call arms the retarget.
	TransportRemoteCDP: {CDPSession: true, BrowserTarget: true},
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
