package extensionbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrRouteResponseBodyUnsupported is returned by the extension-bridge transport
// for any route that has to supply a response body: behaviour=fulfill and the
// HAR replay built on it.
//
// The bridge drives the user's real signed-in Chrome, where interception is
// declarativeNetRequest rather than Fetch: a declarative rule decides whether a
// request happens, and Chrome never hands the extension the response to write.
// CDP's Fetch domain could, but only through the chrome.debugger session the
// extension attaches and detaches around each operation, and interception
// dropped at detach would be worse than an error — the page would silently
// reach the real endpoint while the caller believed it was mocked. So the gap is
// named rather than degraded.
var ErrRouteResponseBodyUnsupported = errors.New("answering a request from a body is not supported on the extension-bridge transport: declarativeNetRequest can refuse a request but cannot supply a response, so brw_route fulfill and HAR replay need a direct-CDP profile; behaviour=abort works here")

// ErrRouteTimesUnsupported is returned for a rule asked to retire itself after a
// fixed number of matches. A declarativeNetRequest rule applies until it is
// removed and the extension is never told a rule fired, so brw cannot count
// matches on this transport; silently ignoring times would leave a caller
// believing one request was blocked when every later one was too.
var ErrRouteTimesUnsupported = errors.New("times is not supported on the extension-bridge transport: a declarativeNetRequest rule applies until it is cleared and reports no match count; use a direct-CDP profile, or clear the route when you are done with it")

// CheckRouteReplay implements browser.RouteReplayer. It always refuses: the
// answer is a property of the transport, not of the request, so the surface
// holding the artifact store can skip reading a recording this bridge would
// reject anyway.
func (b *Bridge) CheckRouteReplay() error {
	return ErrRouteResponseBodyUnsupported
}

// bridgeRouteTable holds the rules the daemon believes are installed, per tab.
// The extension owns the live declarativeNetRequest rule set; this is the copy
// brw_route lists and rebuilds from.
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
//
// Rules are pushed as one complete per-tab set rather than incrementally: the
// extension replaces the tab's session rules wholesale, so a failed push leaves
// the browser and the daemon's table on the same side of the change instead of
// drifting apart.
func (b *Bridge) Route(ctx context.Context, opts browser.RouteOptions) (browser.RouteResult, error) {
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

// bridgeRoute validates one rule against what declarativeNetRequest can express.
func bridgeRoute(opts browser.RouteOptions) (browser.Route, error) {
	pattern := strings.TrimSpace(opts.Pattern)
	if pattern == "" {
		return browser.Route{}, errors.New("route add requires a pattern (a URL glob such as https://api.example.com/*)")
	}
	// The same refusal the direct-CDP backend gives, from the same table: a
	// behaviour brw declines to implement must not read as "not on this
	// transport", which is what would send a caller looking for another profile.
	if err := browser.CheckRouteBehaviourSupported(opts.Behaviour); err != nil {
		return browser.Route{}, err
	}
	switch browser.RouteBehaviour(strings.ToLower(strings.TrimSpace(opts.Behaviour))) {
	case browser.RouteAbort:
	case "", browser.RouteFulfill:
		// An omitted behaviour means fulfill, which is the one this transport
		// cannot do, so the default has to be refused as loudly as the explicit
		// spelling rather than quietly becoming an abort.
		return browser.Route{}, ErrRouteResponseBodyUnsupported
	default:
		return browser.Route{}, fmt.Errorf("unknown route behaviour %q: use abort (fulfill needs a direct-CDP profile)", opts.Behaviour)
	}
	if opts.Times > 0 {
		return browser.Route{}, ErrRouteTimesUnsupported
	}
	return browser.Route{Pattern: pattern, Behaviour: browser.RouteAbort}, nil
}

// pushRoutes replaces the tab's declarativeNetRequest session rules.
func (b *Bridge) pushRoutes(ctx context.Context, tabID string, routes []browser.Route) error {
	rules := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		rules = append(rules, map[string]any{
			"regex":     browser.RoutePatternRegex(route.Pattern),
			"behaviour": string(route.Behaviour),
		})
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
