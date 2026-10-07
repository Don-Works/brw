package mcp

import (
	"sort"

	"github.com/Don-Works/brw/internal/brwidentity"
)

type toolRequirement int

const (
	needsCDPSession toolRequirement = iota

	needsBrowserTarget

	needsExtensionAPIs

	refusedOnSignedInProfile

	needsDownloadRouting

	needsLocalBrowserHost
)

var toolRequirements = map[string][]toolRequirement{
	"brw_open_incognito": {needsBrowserTarget},
	"brw_close_context":  {needsBrowserTarget},

	"brw_cookies": {needsBrowserTarget},

	"brw_clipboard": {needsBrowserTarget, needsLocalBrowserHost},

	"brw_group_tabs":      {needsExtensionAPIs},
	"brw_ungroup_tabs":    {needsExtensionAPIs},
	"brw_list_tab_groups": {needsExtensionAPIs},

	"brw_set_geolocation":        {needsCDPSession},
	"brw_set_network_conditions": {needsCDPSession},
	"brw_emulate_media":          {needsCDPSession},
	"brw_set_locale":             {needsCDPSession},
	"brw_init_script":            {needsCDPSession},

	"brw_touch": {needsCDPSession},

	"brw_profile":           {needsCDPSession},
	"brw_set_extra_headers": {needsCDPSession},
	"brw_set_user_agent":    {needsCDPSession},
	"brw_authenticate":      {needsCDPSession},

	"brw_set_download_path": {needsDownloadRouting, needsLocalBrowserHost},

	"brw_downloads":       {needsLocalBrowserHost},
	"brw_upload_file":     {needsLocalBrowserHost},
	"brw_screenshot_save": {needsLocalBrowserHost},

	"brw_key_down":  {needsCDPSession},
	"brw_key_up":    {needsCDPSession},
	"brw_pushstate": {needsCDPSession},

	"brw_state": {refusedOnSignedInProfile, needsLocalBrowserHost},
}

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

		return false
	}
}

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

func runnableOnAll(reqs []toolRequirement, caps brwidentity.TransportCapabilities) bool {
	for _, req := range reqs {
		if !runnableOn(req, caps) {
			return false
		}
	}
	return true
}

func unsupportedOn(tool, transport string) bool {
	for _, bad := range transportUnsupported[tool] {
		if bad == transport {
			return true
		}
	}
	return false
}
