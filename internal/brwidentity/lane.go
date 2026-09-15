package brwidentity

import (
	"net"
	"net/url"
	"strings"
)

// BrowserRunsOnThisHost answers the question every capability gate in brw is
// really asking: are the profile, the downloads directory, the clipboard and
// the signed-in cookies this browser can reach the ones belonging to the
// machine brwd is running on?
//
// It is asked of the CDP endpoint, not of a flag name, because the flag is not
// the property. --remote was read as "a Chrome on this machine" for as long as
// it existed, and it is a URL: brwd --remote http://198.51.100.7:9222 reaches a
// browser on another machine down the same code path a loopback endpoint takes.
// A gate written as "did the operator ask for a provider?" is inert for it, and
// every verb that resolves a path, a clipboard or this host's session-snapshot
// store then answers about the wrong machine without saying so.
//
// An empty endpoint is this host: brw launched the browser itself.
//
// It fails CLOSED. A host it cannot prove is loopback from the string alone — a
// name that would need a DNS lookup, an endpoint that does not parse — is
// another machine. The cost of a wrong "another machine" is a refused
// capability that names its reason; the cost of a wrong "this machine" is this
// host's session cookies installed into somebody else's browser.
func BrowserRunsOnThisHost(cdpEndpoint string) bool {
	endpoint := strings.TrimSpace(cdpEndpoint)
	if endpoint == "" {
		return true
	}
	host := ""
	if parsed, err := url.Parse(endpoint); err == nil {
		host = parsed.Hostname()
	}
	if host == "" {
		// A bare "127.0.0.1:9222" has no scheme, so url.Parse reads "127.0.0.1"
		// as one and leaves nothing in Host. Read it as an authority instead,
		// rather than declaring an endpoint brw would happily dial unparseable.
		if h, _, err := net.SplitHostPort(endpoint); err == nil {
			host = h
		} else {
			host = endpoint
		}
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Lane is every way a brwd process can be pointed at a browser, as configured
// on its command line. It is a struct rather than a run of arguments so the
// classification below is one function over the whole set: the identity a
// daemon reports, the on-disk scope it resolves, the tools it advertises and
// the capabilities its manager refuses all read the same answer, and cannot
// disagree about which lane they are in.
type Lane struct {
	// UpstreamHTTP is set on a proxy that forwards to a browser-host daemon.
	UpstreamHTTP string
	// Bridge drives the Chrome a human is personally signed into on this
	// machine, through the brw extension.
	Bridge bool
	// BrowserProvider is a browser a plugin holding browser.provider minted,
	// which is on the provider's machine.
	BrowserProvider bool
	// ChromeOptIn drives the Chrome a human is personally signed into on this
	// machine, through the remote debugging they switched on themselves at
	// chrome://inspect. It resolves to a loopback CDPEndpoint, so it is the
	// flag rather than the endpoint that tells this lane apart from --remote at
	// the same port — and they are not the same lane: brw must not seal or
	// retarget what belongs to that browser's own user.
	ChromeOptIn bool
	// CDPEndpoint is what --remote names: a browser brw did not launch, which
	// may be on this machine or on another one. Which it is, is read off the
	// endpoint rather than assumed.
	CDPEndpoint string
}

// Transport classifies a lane by where the browser is and how brw reaches it.
//
// The fall-through is the RESTRICTIVE answer. A lane that names an endpoint brw
// cannot prove is on this machine is off-host-cdp with no edit here, which is
// what makes the gate survive a new way of naming a browser: off-host-cdp
// refuses this host's files, clipboard and session store by name instead of
// handing them to whatever is on the other end of the socket. A new lane FIELD
// still has to be classified, and TestEveryLaneFieldChangesTheClassification is
// what fails when one is added and forgotten.
//
// The off-host test runs BEFORE the opt-in and --remote cases on purpose. Both
// of those describe a browser on this machine, so a lane that somehow carried
// one of their flags alongside an endpoint elsewhere must answer "elsewhere":
// the cost of a wrong "this machine" is this host's session cookies installed
// into somebody else's browser, and the cost of a wrong "elsewhere" is a
// refused capability that names its reason.
//
// The empty string is not a transport. A proxy cannot know how its upstream
// reaches Chrome, so it says nothing here and adopts the upstream's answer from
// /health.
func (l Lane) Transport() string {
	switch {
	case strings.TrimSpace(l.UpstreamHTTP) != "":
		return ""
	case l.Bridge:
		return TransportExtensionBridge
	case l.BrowserProvider:
		return TransportOffHostCDP
	case !BrowserRunsOnThisHost(l.CDPEndpoint):
		return TransportOffHostCDP
	case l.ChromeOptIn:
		return TransportChromeOptIn
	case strings.TrimSpace(l.CDPEndpoint) != "":
		// A browser on this machine that brw did not start. It keeps every
		// local-machine capability and loses only the one that depends on brw
		// having started the browser: routing its downloads.
		return TransportRemoteCDP
	default:
		return TransportDirectCDP
	}
}

// BrowserOnThisHost reports whether the browser at the end of this lane is on
// the machine brwd runs on. The extension bridge, a direct-CDP launch, the
// Chrome opt-in and --remote at a loopback endpoint all are; a provider's
// browser and a --remote endpoint elsewhere are not.
//
// It asks the transport rather than the flags so this and the tool catalogue
// cannot disagree: off-host-cdp is exactly the set of lanes whose browser is
// somewhere else, and TransportCapabilities.BrowserOnThisHost says the same
// thing per transport.
//
// A proxy answers true: it drives no browser itself, and the daemon it forwards
// to applies its own lane's answer.
func (l Lane) BrowserOnThisHost() bool {
	return l.Transport() != TransportOffHostCDP
}
