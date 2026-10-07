package brwidentity

import (
	"net"
	"net/url"
	"strings"
)

// BrowserRunsOnThisHost answers the question every capability gate in brw is really asking: are the profile, the downloads directory, the clipboard and the signed-in cookies this browser can reach the ones belonging to the machine brwd is running on?
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

// Lane is every way a brwd process can be pointed at a browser, as configured on its command line.
type Lane struct {
	// UpstreamHTTP is set on a proxy that forwards to a browser-host daemon.
	UpstreamHTTP string
	// Bridge drives the Chrome a human is personally signed into on this machine, through the brw extension.
	Bridge bool
	// BrowserProvider is a browser a plugin holding browser.provider minted, which is on the provider's machine.
	BrowserProvider bool
	// ChromeOptIn drives the Chrome a human is personally signed into on this machine, through the remote debugging they switched on themselves at chrome://inspect.
	ChromeOptIn bool
	// CDPEndpoint is what --remote names: a browser brw did not launch, which may be on this machine or on another one.
	CDPEndpoint string
}

// Transport classifies a lane by where the browser is and how brw reaches it.
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

		return TransportRemoteCDP
	default:
		return TransportDirectCDP
	}
}

// BrowserOnThisHost reports whether the browser at the end of this lane is on the machine brwd runs on.
func (l Lane) BrowserOnThisHost() bool {
	return l.Transport() != TransportOffHostCDP
}
