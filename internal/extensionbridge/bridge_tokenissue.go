package extensionbridge

import (
	"net/http"
	"strings"
)

// handshakeReport is the endpoint config a refused handshake reported, published
// on /status as "last_handshake". A refused connection never becomes b.hello, so
// this is the only place the URL the extension actually tried is recorded
// anywhere outside the browser's own storage. It carries no secret: the token is
// zeroed in verifyHandshake before the hello is returned.
//
// It deliberately survives a later successful connection, unlike
// disconnectReason: an extension that is refused, reloaded and then accepted is
// a machine whose config was just fixed, and the refusals are what explain a
// bridge that keeps cycling. "last" and the At timestamp are what say it is
// history — a reader that wants the CURRENT endpoint reads hello.status_url,
// which is only set while something is connected.
type handshakeReport struct {
	StatusURL    string `json:"status_url,omitempty"`
	BridgeURL    string `json:"bridge_url,omitempty"`
	ConfigSource string `json:"config_source,omitempty"`
	Reason       string `json:"reason,omitempty"`
	At           string `json:"at,omitempty"`
}

func (h handshakeReport) Empty() bool {
	return h.StatusURL == "" && h.BridgeURL == "" && h.ConfigSource == "" && h.Reason == ""
}

// handshakeFieldLimit bounds one recorded field. A status URL is ~40 bytes; this
// is generous for a real one and small enough that the record cannot be used as
// storage.
const handshakeFieldLimit = 256

// sanitizeHandshakeField strips control characters and truncates. What this
// records arrives on an UNAUTHENTICATED connection — the whole point is to keep
// what a client said before it was turned away — and `brwctl doctor` prints it
// straight to a terminal. Without this, a rogue local client chooses the escape
// sequences in an operator's diagnostic output.
//
// The accepted hello is not filtered the same way: by then the client has
// presented the per-launch token, and its other fields (the Chrome UA, the
// profile label) have always been echoed as sent.
func sanitizeHandshakeField(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	// Truncate by rune, not by byte: a byte cut through a multi-byte character
	// would put a replacement character in the report and make the URL it names
	// unrecognisable.
	if runes := []rune(cleaned); len(runes) > handshakeFieldLimit {
		return string(runes[:handshakeFieldLimit])
	}
	return cleaned
}

// tokenServable reports whether the handshake token may be included in a /status
// response: a loopback Host, and an Origin that is either absent or exactly this
// bridge's configured extension.
//
// Be precise about what that buys, because it is less than it looks.
//
// The absent-Origin case is the extension. Measured on Chromium 152, an MV3
// service worker fetching a loopback URL it holds host_permissions for sends NO
// Origin header at all — the request arrives with Sec-Fetch-Site: none and no
// initiator origin. Requiring the header would refuse the only client this
// endpoint exists for. It also means no real browser client reaches here WITH an
// Origin unless it lacks the host permission, in which case CORS already denies
// it the body. So the exact match is hygiene — the class of prefix-match-on-
// attacker-input bug this codebase has been bitten by, removed — and not a new
// boundary: it closes no path that was reachable.
//
// The Host check is load-bearing: a DNS-rebinding page reaches the daemon with
// an attacker Host and no Origin, and this is what excludes it.
//
// Who still gets the token: any local process, and any other extension holding
// loopback host permissions. Neither can be told apart from the real extension
// by anything in the request. What stops an extension using it is
// handleExtension's Origin pin, which the browser sets and no page or extension
// can forge (TestConfiguredExtensionOriginAcceptedAndOthersRejected); what stops
// a local process is nothing. docs/auth-model.md argues that boundary rather
// than pretending this function moves it.
func (b *Bridge) tokenServable(r *http.Request) bool {
	if !isLoopbackHostname(r.Host) {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if allowed := b.effectiveExtensionID(); allowed != "" {
		return origin == "chrome-extension://"+allowed
	}
	// No extension id is configured anywhere (not even the published default),
	// so there is nothing to pin to; a well-formed extension origin is all that
	// can be required. handleExtension logs a warning in this same state.
	return extensionOriginOK(origin)
}
