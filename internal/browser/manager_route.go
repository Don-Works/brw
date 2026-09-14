package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

// maxRoutesPerTab bounds the route table so a runaway caller cannot make every
// request walk an unbounded list.
const maxRoutesPerTab = 50

// RouteBehaviour selects what a matching request does instead of reaching the
// network.
type RouteBehaviour string

const (
	// RouteAbort fails the request as if the network refused it.
	RouteAbort RouteBehaviour = "abort"
	// RouteFulfill answers the request from the route's own body/status.
	RouteFulfill RouteBehaviour = "fulfill"
)

// Route is one interception rule.
type Route struct {
	// Pattern is a glob against the full URL: "*" matches any run of characters.
	// "https://api.example.com/v1/*" is the common shape.
	Pattern     string            `json:"pattern"`
	Behaviour   RouteBehaviour    `json:"behaviour"`
	Status      int               `json:"status,omitempty"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// Times, when positive, retires the route after that many matches. Zero
	// means it applies until cleared.
	Times int `json:"times,omitempty"`
	// Matched counts how often the route fired, so a test can tell "the mock
	// answered" from "the request never happened".
	Matched int `json:"matched"`
}

// RouteResult is the brw_route reply.
type RouteResult struct {
	Action string  `json:"action"`
	TabID  string  `json:"tab_id,omitempty"`
	Routes []Route `json:"routes"`
	Count  int     `json:"count"`
	Note   string  `json:"note,omitempty"`
}

type routeState struct {
	mu     sync.Mutex
	routes map[string][]*Route
}

func (r *routeState) initLocked() {
	if r.routes == nil {
		r.routes = make(map[string][]*Route)
	}
}

func (r *routeState) list(tabID string) []Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	out := make([]Route, 0, len(r.routes[tabID]))
	for _, route := range r.routes[tabID] {
		out = append(out, *route)
	}
	return out
}

// count reports how many rules are active for a tab.
func (r *routeState) count(tabID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.routes[tabID])
}

// match returns the first route whose pattern matches, consuming one of its
// remaining uses. First-match-wins makes the table readable: a specific rule
// added before a general one takes precedence.
func (r *routeState) match(tabID, url string) *Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	routes := r.routes[tabID]
	for i, route := range routes {
		if !matchURLGlob(route.Pattern, url) {
			continue
		}
		route.Matched++
		hit := *route
		if route.Times > 0 && route.Matched >= route.Times {
			r.routes[tabID] = append(routes[:i:i], routes[i+1:]...)
		}
		return &hit
	}
	return nil
}

// remove drops one rule by identity, so an add whose interception could not be
// armed leaves no route behind that would never fire.
func (r *routeState) remove(tabID string, target *Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	routes := r.routes[tabID]
	for i, route := range routes {
		if route != target {
			continue
		}
		r.routes[tabID] = append(routes[:i:i], routes[i+1:]...)
		return
	}
}

// matchURLGlob matches a URL against a "*" glob. path.Match is not used: it
// treats "/" as a separator, so "https://host/*" would fail to match a nested
// path, which is the opposite of what a URL pattern means to a caller.
func matchURLGlob(pattern, url string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		// A pattern with no wildcard matches as a prefix, so a caller can name an
		// endpoint without having to remember its query string.
		return strings.HasPrefix(url, pattern)
	}
	parts := strings.Split(pattern, "*")
	rest := url
	if parts[0] != "" {
		if !strings.HasPrefix(rest, parts[0]) {
			return false
		}
		rest = rest[len(parts[0]):]
	}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		index := strings.Index(rest, part)
		if index < 0 {
			return false
		}
		rest = rest[index+len(part):]
	}
	// A trailing non-empty segment must end the URL.
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(url, last) {
		return false
	}
	return true
}

// Route adds, lists or clears interception rules for a tab.
func (m *Manager) Route(ctx context.Context, opts RouteOptions) (RouteResult, error) {
	tabID := strings.TrimSpace(opts.TabID)
	if tabID == "" {
		active, err := m.ensureActive(ctx)
		if err != nil {
			return RouteResult{}, err
		}
		tabID = active
	}
	tabCtx, err := m.tabContext(tabID)
	if err != nil {
		return RouteResult{}, err
	}

	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "list":
		routes := m.routes.list(tabID)
		return RouteResult{Action: "list", TabID: tabID, Routes: routes, Count: len(routes)}, nil

	case "add":
		route, err := buildRoute(opts)
		if err != nil {
			return RouteResult{}, err
		}
		m.routes.mu.Lock()
		m.routes.initLocked()
		if len(m.routes.routes[tabID]) >= maxRoutesPerTab {
			m.routes.mu.Unlock()
			return RouteResult{}, fmt.Errorf("tab already has the maximum of %d routes; clear some first", maxRoutesPerTab)
		}
		m.routes.routes[tabID] = append(m.routes.routes[tabID], route)
		m.routes.mu.Unlock()
		// Routes need request interception even when no navigation policy is
		// configured, so arm it here rather than only at containment time.
		m.armInterception(tabID, tabCtx)
		// armInterception installs the tab's listener once and returns early ever
		// after, so on a tab where interception was later turned back OFF — by a
		// finished brw_authenticate or by brw_set_extra_headers{clear:true} — this
		// sync is the only thing that turns it back on. Without it the route is
		// accepted and listed while every matching request goes to the real
		// network, which is a mocked test reporting green against production.
		if err := m.syncFetchInterception(tabCtx, tabID); err != nil {
			m.routes.remove(tabID, route)
			return RouteResult{}, fmt.Errorf("arm request interception for this route: %w", err)
		}
		routes := m.routes.list(tabID)
		return RouteResult{
			Action: "add", TabID: tabID, Routes: routes, Count: len(routes),
			Note: "requests matching this pattern no longer reach the network; active routes are reported in brw_observe so mocked traffic is never invisible",
		}, nil

	case "clear":
		m.routes.mu.Lock()
		m.routes.initLocked()
		if strings.TrimSpace(opts.Pattern) == "" {
			delete(m.routes.routes, tabID)
		} else {
			kept := m.routes.routes[tabID][:0]
			for _, route := range m.routes.routes[tabID] {
				if route.Pattern != opts.Pattern {
					kept = append(kept, route)
				}
			}
			m.routes.routes[tabID] = kept
		}
		m.routes.mu.Unlock()
		routes := m.routes.list(tabID)
		result := RouteResult{Action: "clear", TabID: tabID, Routes: routes, Count: len(routes)}
		// The route table is usually the only reason this tab was intercepting;
		// left armed, every later request still pauses and crosses to the daemon
		// for nothing. Whether it actually goes off is syncFetchInterception's
		// decision: a credential, a header table or the navigation policy keeps it.
		if err := m.syncFetchInterception(tabCtx, tabID); err != nil {
			result.Note = "the routes are cleared; request interception could not be turned back off, so requests on this tab still pause at the daemon: " + err.Error()
		}
		return result, nil

	default:
		return RouteResult{}, fmt.Errorf("unknown route action %q: use add, list, or clear", opts.Action)
	}
}

// RouteOptions selects one brw_route operation.
type RouteOptions struct {
	Action      string            `json:"action"`
	Pattern     string            `json:"pattern"`
	Behaviour   string            `json:"behaviour"`
	Status      int               `json:"status"`
	Body        string            `json:"body"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Times       int               `json:"times"`
	TabID       string            `json:"tab_id"`
}

func buildRoute(opts RouteOptions) (*Route, error) {
	pattern := strings.TrimSpace(opts.Pattern)
	if pattern == "" {
		return nil, fmt.Errorf("route add requires a pattern (a URL glob such as https://api.example.com/*)")
	}
	behaviour := RouteBehaviour(strings.ToLower(strings.TrimSpace(opts.Behaviour)))
	switch behaviour {
	case "":
		behaviour = RouteFulfill
	case RouteAbort, RouteFulfill:
	default:
		return nil, fmt.Errorf("unknown route behaviour %q: use abort or fulfill", opts.Behaviour)
	}
	route := &Route{
		Pattern:     pattern,
		Behaviour:   behaviour,
		Status:      opts.Status,
		Body:        opts.Body,
		ContentType: opts.ContentType,
		Headers:     opts.Headers,
		Times:       opts.Times,
	}
	if behaviour == RouteFulfill {
		if route.Status == 0 {
			route.Status = 200
		}
		if route.ContentType == "" {
			route.ContentType = guessContentType(pattern, route.Body)
		}
	}
	return route, nil
}

// guessContentType picks a sensible default so the common case (mock a JSON
// endpoint) does not require naming the type.
func guessContentType(pattern, body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "application/json"
	}
	switch strings.ToLower(path.Ext(strings.SplitN(pattern, "?", 2)[0])) {
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".js":
		return "text/javascript"
	case ".css":
		return "text/css"
	}
	return "text/plain"
}

// applyRoute answers an intercepted request from a route.
func applyRoute(ctx context.Context, route *Route, requestID fetch.RequestID) error {
	if route.Behaviour == RouteAbort {
		return fetch.FailRequest(requestID, network.ErrorReasonFailed).Do(ctx)
	}
	headers := []*fetch.HeaderEntry{{Name: "Content-Type", Value: route.ContentType}}
	for name, value := range route.Headers {
		headers = append(headers, &fetch.HeaderEntry{Name: name, Value: value})
	}
	status := int64(route.Status)
	return fetch.FulfillRequest(requestID, status).
		WithResponseHeaders(headers).
		WithBody(base64.StdEncoding.EncodeToString([]byte(route.Body))).
		Do(ctx)
}
