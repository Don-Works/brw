package browser

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/chromedp/cdproto/network"
)

var routeResourceTypes = map[string]bool{
	"main_frame":     true,
	"sub_frame":      true,
	"stylesheet":     true,
	"script":         true,
	"image":          true,
	"font":           true,
	"object":         true,
	"xmlhttprequest": true,
	"ping":           true,
	"csp_report":     true,
	"media":          true,
	"websocket":      true,
	"other":          true,
}

// RouteResourceTypeNames lists the accepted resource-type names.
func RouteResourceTypeNames() []string {
	return slices.Sorted(maps.Keys(routeResourceTypes))
}

// NormalizeResourceTypes validates and lowercases a resource-type filter.
func NormalizeResourceTypes(types []string) ([]string, error) {
	if len(types) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(types))
	seen := map[string]bool{}
	for _, raw := range types {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}

		if name == "document" {
			for _, alias := range []string{"main_frame", "sub_frame"} {
				if !seen[alias] {
					seen[alias] = true
					out = append(out, alias)
				}
			}
			continue
		}
		if !routeResourceTypes[name] {
			return nil, fmt.Errorf("unknown resource type %q: use one of %s", raw, strings.Join(RouteResourceTypeNames(), ", "))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func resourceTypeMatches(filter []string, resource network.ResourceType) bool {
	if len(filter) == 0 {
		return true
	}
	canonical := resourceTypeCanonical(resource)
	for _, name := range filter {
		if name == canonical {
			return true
		}

		if canonical == "main_frame" && name == "sub_frame" {
			return true
		}
	}
	return false
}

func resourceTypeCanonical(resource network.ResourceType) string {
	switch resource {
	case network.ResourceTypeDocument:
		return "main_frame"
	case network.ResourceTypeStylesheet:
		return "stylesheet"
	case network.ResourceTypeScript:
		return "script"
	case network.ResourceTypeImage:
		return "image"
	case network.ResourceTypeFont:
		return "font"
	case network.ResourceTypeXHR, network.ResourceTypeFetch:
		return "xmlhttprequest"
	case network.ResourceTypePing:
		return "ping"
	case network.ResourceTypeCSPViolationReport:
		return "csp_report"
	case network.ResourceTypeMedia:
		return "media"
	case network.ResourceTypeWebSocket:
		return "websocket"
	default:
		return "other"
	}
}
