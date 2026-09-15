package extensionbridge

import (
	"net/http"
	"strings"
	"unicode/utf8"
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
	// Cut to bytes BEFORE mapping. This runs before the client has presented
	// anything and the websocket read limit is 4 MiB, so mapping first would let
	// a caller choose a multi-megabyte string copy plus a []rune four times that
	// size, per field, per refused handshake, for a record that is then cut to
	// handshakeFieldLimit runes anyway. That many runes cannot occupy more than
	// handshakeFieldLimit*utf8.UTFMax bytes, so nothing that survives is lost.
	if len(value) > handshakeFieldLimit*utf8.UTFMax {
		value = trimPartialRune(value[:handshakeFieldLimit*utf8.UTFMax])
	}
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

// trimPartialRune drops trailing bytes that do not decode, so the byte cut above
// cannot turn a multi-byte character into a replacement character that the rune
// truncation would then keep.
func trimPartialRune(value string) string {
	for value != "" {
		r, size := utf8.DecodeLastRuneInString(value)
		if r != utf8.RuneError || size > 1 {
			return value
		}
		value = value[:len(value)-1]
	}
	return value
}

// initiatorSecFetchSite is the only Sec-Fetch-Site value a brw browser client
// produces. Measured on Chromium 152.0.7977.82 on 2026-09-15: both the MV3
// service worker's fetch and an extension page's fetch of a loopback URL the
// extension holds host_permissions for arrive as
// `Sec-Fetch-Site: none, Sec-Fetch-Mode: cors` with no Origin.
const initiatorSecFetchSite = "none"

// tokenServable reports whether the handshake token may be included in a /status
// response: a loopback Host, an Origin that is either absent or exactly this
// bridge's configured extension, and — when the caller is a browser — an
// initiator that is not another document.
//
// Be precise about what that buys, because it is less than it looks.
//
// The absent-Origin case is the extension. Measured on Chromium 152.0.7977.82 on
// 2026-09-15, an MV3 service worker fetching a loopback URL it holds
// host_permissions for sends NO Origin header at all. Requiring the header would
// refuse the only client this endpoint exists for. It also means no real browser
// client reaches here WITH an Origin unless it lacks the host permission, in
// which case CORS already denies it the body. So the exact match is hygiene —
// the class of prefix-match-on-attacker-input bug this codebase has been bitten
// by, removed — and not a new boundary.
//
// Sec-Fetch-Site is a boundary, for browser callers only. The same measurement
// found two shapes that reach here from a page on ANOTHER site with no Origin at
// all — `fetch(url, {mode:"no-cors"})` and `<script src=url>`, both
// `Sec-Fetch-Site: cross-site` — because a no-cors GET carries no Origin. Those
// were served before this check. The page could not read the reply (opaque
// response, and the JSON body is not a parseable script), but a page on the
// internet causing a request that answers with the token is not a thing to leave
// in place on the strength of the reply being unreadable. A browser sets
// Sec-Fetch-* itself and forbids page script from overriding it, so
// `Sec-Fetch-Site: none` is a property a web page cannot present. Measured by
// TestMV3ServiceWorkerAndWebPageStatusHeadersAreMeasured, which drives a real
// browser and then feeds the headers it observed back through this function.
//
// An absent Sec-Fetch-Site is still served, because that is a non-browser local
// client (brwctl doctor sends none) — and any local process can send whatever
// header it likes, so requiring the header would refuse a real caller and
// inconvenience no attacker.
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
	// Checked before the Origin cases rather than inside the empty-Origin one:
	// the property is "no document initiated this", and a caller that sends both
	// an extension Origin and a cross-site Sec-Fetch-Site is describing two
	// different initiators, which no browser produces.
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" && site != initiatorSecFetchSite {
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
