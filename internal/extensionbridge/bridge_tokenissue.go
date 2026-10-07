package extensionbridge

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

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

const handshakeFieldLimit = 256

func sanitizeHandshakeField(value string) string {
	if len(value) > handshakeFieldLimit*utf8.UTFMax {
		value = trimPartialRune(value[:handshakeFieldLimit*utf8.UTFMax])
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)

	if runes := []rune(cleaned); len(runes) > handshakeFieldLimit {
		return string(runes[:handshakeFieldLimit])
	}
	return cleaned
}

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

const initiatorSecFetchSite = "none"

func (b *Bridge) tokenServable(r *http.Request) bool {
	if !isLoopbackHostname(r.Host) {
		return false
	}

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

	return extensionOriginOK(origin)
}
