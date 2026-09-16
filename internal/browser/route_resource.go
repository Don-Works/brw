package browser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/network"
)

// routeResourceTypes is the canonical vocabulary for a route's resource_types.
//
// The names are Chrome's declarativeNetRequest resource types, which is also the
// vocabulary agent-browser's `--resource-type` flag uses, so a caller moving
// between the tools can keep the same words. On direct CDP they are matched
// against the paused request's DevTools Protocol resource type through
// resourceTypeCanonical below; a DevTools type with no distinct DNR name folds
// into "other".
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
	names := make([]string, 0, len(routeResourceTypes))
	for name := range routeResourceTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NormalizeResourceTypes validates and lowercases a resource-type filter. An
// empty list means "every kind", which is the pre-existing behaviour.
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
		// "document" is the DevTools Protocol name a caller is most likely to
		// reach for; accept it as the pair of DNR frame types rather than
		// answering with a puzzling list of valid names.
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

// resourceTypeMatches reports whether a paused request's DevTools resource type
// is one the filter names. An empty filter matches everything.
func resourceTypeMatches(filter []string, resource network.ResourceType) bool {
	if len(filter) == 0 {
		return true
	}
	canonical := resourceTypeCanonical(resource)
	for _, name := range filter {
		if name == canonical {
			return true
		}
		// A top-level document and a subframe document are both ResourceTypeDocument
		// on the paused request, so a filter naming either frame kind matches both.
		if canonical == "main_frame" && name == "sub_frame" {
			return true
		}
	}
	return false
}

// resourceTypeCanonical folds a DevTools Protocol resource type into the
// canonical vocabulary. A type with no distinct DNR name becomes "other".
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
