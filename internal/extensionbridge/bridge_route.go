package extensionbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrRouteResponseBodyUnsupported is returned by the extension-bridge transport for any route that has to supply a response body: behaviour=fulfill and the HAR replay built on it.
var ErrRouteResponseBodyUnsupported = errors.New("answering a request from a body is not supported on the extension-bridge transport: declarativeNetRequest can refuse a request but cannot supply a response, so brw_route fulfill and HAR replay need a direct-CDP profile; behaviour=abort works here")

// ErrRouteTimesUnsupported is returned for a rule asked to retire itself after a fixed number of matches.
var ErrRouteTimesUnsupported = errors.New("times is not supported on the extension-bridge transport: a declarativeNetRequest rule applies until it is cleared and reports no match count; use a direct-CDP profile, or clear the route when you are done with it")

// CheckRouteReplay implements browser.RouteReplayer.
func (b *Bridge) CheckRouteReplay() error {
	return ErrRouteResponseBodyUnsupported
}

type bridgeRouteTable struct {
	mu     sync.Mutex
	routes map[string][]browser.Route
}

func (t *bridgeRouteTable) list(tabID string) []browser.Route {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]browser.Route(nil), t.routes[tabID]...)
}

func (t *bridgeRouteTable) set(tabID string, routes []browser.Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.routes == nil {
		t.routes = make(map[string][]browser.Route)
	}
	if len(routes) == 0 {
		delete(t.routes, tabID)
		return
	}
	t.routes[tabID] = routes
}

// Route implements the RouteController capability over declarativeNetRequest.
func (b *Bridge) Route(ctx context.Context, opts browser.RouteOptions) (browser.RouteResult, error) {
	if err := browser.CheckRouteBehaviourSupported(opts.Behaviour); err != nil {
		return browser.RouteResult{}, err
	}
	tabID := strings.TrimSpace(opts.TabID)
	if tabID == "" {
		tabID = b.contextTabID(ctx)
	}
	if tabID == "" {
		return browser.RouteResult{}, errors.New("no tab to route: open one with brw_open first, or pass tab_id")
	}

	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "list":
		routes := b.routes.list(tabID)
		return browser.RouteResult{Action: "list", TabID: tabID, Routes: routes, Count: len(routes)}, nil

	case "replay":
		return browser.RouteResult{}, ErrRouteResponseBodyUnsupported

	case "add":
		route, err := bridgeRoute(opts)
		if err != nil {
			return browser.RouteResult{}, err
		}
		existing := b.routes.list(tabID)
		if len(existing) >= browser.MaxRoutesPerTab {
			return browser.RouteResult{}, fmt.Errorf("tab already has the maximum of %d routes; clear some first", browser.MaxRoutesPerTab)
		}
		next := append(existing, route)
		if err := b.pushRoutes(ctx, tabID, next); err != nil {
			return browser.RouteResult{}, err
		}
		b.routes.set(tabID, next)
		return browser.RouteResult{
			Action: "add", TabID: tabID, Routes: next, Count: len(next),
			Note: "requests matching this pattern are refused by a declarativeNetRequest session rule scoped to this tab; the rule applies until cleared and is dropped when the tab closes",
		}, nil

	case "clear":
		existing := b.routes.list(tabID)
		var next []browser.Route
		if pattern := strings.TrimSpace(opts.Pattern); pattern != "" {
			for _, route := range existing {
				if route.Pattern != pattern {
					next = append(next, route)
				}
			}
		}
		if err := b.pushRoutes(ctx, tabID, next); err != nil {
			return browser.RouteResult{}, err
		}
		b.routes.set(tabID, next)
		if next == nil {
			next = []browser.Route{}
		}
		return browser.RouteResult{Action: "clear", TabID: tabID, Routes: next, Count: len(next)}, nil

	default:
		return browser.RouteResult{}, fmt.Errorf("unknown route action %q: use add, list, or clear", opts.Action)
	}
}

func bridgeRoute(opts browser.RouteOptions) (browser.Route, error) {
	pattern := strings.TrimSpace(opts.Pattern)
	if pattern == "" {
		return browser.Route{}, errors.New("route add requires a pattern (a URL glob such as https://api.example.com/*)")
	}

	switch browser.RouteBehaviour(strings.ToLower(strings.TrimSpace(opts.Behaviour))) {
	case browser.RouteAbort:
	case "", browser.RouteFulfill:

		return browser.Route{}, ErrRouteResponseBodyUnsupported
	default:
		return browser.Route{}, fmt.Errorf("unknown route behaviour %q: use abort (fulfill needs a direct-CDP profile)", opts.Behaviour)
	}
	if opts.Times > 0 {
		return browser.Route{}, ErrRouteTimesUnsupported
	}
	resourceTypes, err := browser.NormalizeResourceTypes(opts.ResourceTypes)
	if err != nil {
		return browser.Route{}, err
	}
	return browser.Route{Pattern: pattern, Behaviour: browser.RouteAbort, ResourceTypes: resourceTypes}, nil
}

func (b *Bridge) pushRoutes(ctx context.Context, tabID string, routes []browser.Route) error {
	rules := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		rule := map[string]any{
			"regex":     browser.RoutePatternRegex(route.Pattern),
			"behaviour": string(route.Behaviour),
		}

		if len(route.ResourceTypes) > 0 {
			rule["resourceTypes"] = route.ResourceTypes
		}
		rules = append(rules, rule)
	}
	_, err := b.call(ctx, "set_routes", map[string]any{
		"tabId": parseTabID(tabID),
		"rules": rules,
	})
	if isUnknownMessageTypeErr(err) {
		return errors.New("the connected brw extension predates request interception; update it from the extension page and reload the extension")
	}
	return err
}
