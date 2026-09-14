package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

// MaxRoutesPerTab bounds the route table so a runaway caller cannot make every
// request walk an unbounded list. Exported because the extension-bridge
// transport keeps its own table and has to bound it the same way.
const MaxRoutesPerTab = 50

// RouteBehaviour selects what a matching request does instead of reaching the
// network.
type RouteBehaviour string

const (
	// RouteAbort fails the request as if the network refused it.
	RouteAbort RouteBehaviour = "abort"
	// RouteFulfill answers the request from the route's own body/status.
	RouteFulfill RouteBehaviour = "fulfill"
	// RouteReplay answers the request from a recorded HAR.
	RouteReplay RouteBehaviour = "replay"
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
	// Fixture describes the HAR behind a replay rule. Filled only when a route
	// is reported; the recorded exchanges themselves live in har.
	Fixture *RouteFixture `json:"fixture,omitempty"`

	har *harFixture
}

// view copies a stored route into the form a reply carries: the HAR's live
// counters snapshotted, the fixture itself left behind.
func (r *Route) view() Route {
	out := *r
	out.har = nil
	if r.har != nil {
		fixture := r.har.view()
		out.Fixture = &fixture
	}
	return out
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
		out = append(out, route.view())
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

// RoutePatternRegex renders a brw route pattern as an anchored regular
// expression with the same meaning as matchURLGlob.
//
// The extension-bridge transport enforces routes with declarativeNetRequest,
// whose urlFilter is a substring-with-wildcards language rather than brw's, so
// the rule is expressed as a regexFilter instead. Deriving it here, next to the
// matcher it has to agree with, is what keeps one pattern from meaning two
// different things depending on which transport a namespace resolved to.
func RoutePatternRegex(pattern string) string {
	if pattern == "" || pattern == "*" {
		return ".*"
	}
	if !strings.Contains(pattern, "*") {
		return "^" + regexp.QuoteMeta(pattern)
	}
	parts := strings.Split(pattern, "*")
	var out strings.Builder
	out.WriteString("^")
	out.WriteString(regexp.QuoteMeta(parts[0]))
	for i := 1; i < len(parts); i++ {
		out.WriteString(".*")
		out.WriteString(regexp.QuoteMeta(parts[i]))
	}
	// A trailing non-empty segment must end the URL; a trailing "*" leaves the
	// tail open.
	if parts[len(parts)-1] != "" {
		out.WriteString("$")
	}
	return out.String()
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
		routes, err := m.installRoute(tabID, tabCtx, route)
		if err != nil {
			return RouteResult{}, err
		}
		return RouteResult{
			Action: "add", TabID: tabID, Routes: routes, Count: len(routes),
			Note: "requests matching this pattern no longer reach the network; active routes are reported in brw_observe so mocked traffic is never invisible",
		}, nil

	case "replay":
		route, err := buildReplayRoute(opts)
		if err != nil {
			return RouteResult{}, err
		}
		routes, err := m.installRoute(tabID, tabCtx, route)
		if err != nil {
			return RouteResult{}, err
		}
		note := fmt.Sprintf("requests matching this pattern are answered from the %d recorded entries in %s; anything the HAR does not hold goes to the real network",
			len(route.har.entries), route.har.artifactID)
		if route.har.onMiss == HARMissFail {
			note = fmt.Sprintf("requests matching this pattern are answered from the %d recorded entries in %s; anything the HAR does not hold is refused and reported in fixture.misses, so the page cannot reach the network behind the fixture",
				len(route.har.entries), route.har.artifactID)
		}
		return RouteResult{
			Action: "replay", TabID: tabID, Routes: routes, Count: len(routes), Note: note,
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
		return RouteResult{}, fmt.Errorf("unknown route action %q: use add, replay, list, or clear", opts.Action)
	}
}

// installRoute appends a rule and makes sure the tab is actually intercepting.
func (m *Manager) installRoute(tabID string, tabCtx context.Context, route *Route) ([]Route, error) {
	m.routes.mu.Lock()
	m.routes.initLocked()
	if len(m.routes.routes[tabID]) >= MaxRoutesPerTab {
		m.routes.mu.Unlock()
		return nil, fmt.Errorf("tab already has the maximum of %d routes; clear some first", MaxRoutesPerTab)
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
		return nil, fmt.Errorf("arm request interception for this route: %w", err)
	}
	return m.routes.list(tabID), nil
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

	// HARArtifactID names the recorded HAR a replay answers from.
	HARArtifactID string `json:"har_artifact_id"`
	// Match lists the request properties an entry has to agree on. Empty means
	// method and URL.
	Match []string `json:"match"`
	// OnMiss decides what a request the HAR does not hold does.
	OnMiss string `json:"on_miss"`
	// HAR carries the decoded recording. It is never part of the wire schema:
	// the surface holding the artifact store reads the HAR and fills this in, so
	// the transports need no access to artifact storage.
	HAR []HAREntry `json:"-"`
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

// buildReplayRoute turns a recorded HAR into an interception rule.
//
// The pattern scopes the fixture rather than the fixture scoping itself: a HAR
// holds the whole page's traffic, and a caller usually wants only part of it
// replayed ("*/api/*") while the document and its assets still load normally.
// The default "*" is the deterministic-fixture case.
func buildReplayRoute(opts RouteOptions) (*Route, error) {
	artifactID := strings.TrimSpace(opts.HARArtifactID)
	if artifactID == "" {
		return nil, errors.New("route replay requires har_artifact_id, the id of a HAR captured with brw_artifact_capture{kind:\"har\"}")
	}
	if len(opts.HAR) == 0 {
		return nil, fmt.Errorf("HAR artifact %s holds no recorded requests to replay", artifactID)
	}
	if strings.TrimSpace(opts.Behaviour) != "" {
		return nil, fmt.Errorf("route replay answers from the HAR, so behaviour %q does not apply; use action add for a hand-written mock", opts.Behaviour)
	}
	match, err := NormalizeHARMatch(opts.Match)
	if err != nil {
		return nil, err
	}
	onMiss, err := NormalizeHARMiss(opts.OnMiss)
	if err != nil {
		return nil, err
	}
	pattern := strings.TrimSpace(opts.Pattern)
	if pattern == "" {
		pattern = "*"
	}
	return &Route{
		Pattern:   pattern,
		Behaviour: RouteReplay,
		Times:     opts.Times,
		har:       newHARFixture(artifactID, opts.HAR, match, onMiss),
	}, nil
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

// answerRoute answers one intercepted request from a route.
//
// A replay that misses with on_miss:"passthrough" falls back to the ordinary
// continue rather than to a bare fetch.ContinueRequest, so a tab that also has
// per-origin extra headers keeps them on requests the fixture did not record.
func (m *Manager) answerRoute(ctx context.Context, tabID string, route *Route, paused *fetch.EventRequestPaused) error {
	switch route.Behaviour {
	case RouteAbort:
		return fetch.FailRequest(paused.RequestID, network.ErrorReasonFailed).Do(ctx)
	case RouteReplay:
		entry, found := route.har.find(harMethod(paused.Request.Method), paused.Request.URL, requestPostData(paused.Request))
		if !found {
			route.har.recordMiss(paused.Request.Method, paused.Request.URL)
			if route.har.onMiss == HARMissFail {
				return fetch.FailRequest(paused.RequestID, network.ErrorReasonFailed).Do(ctx)
			}
			return m.continueWithEnvironmentHeaders(ctx, tabID, paused)
		}
		return fulfillFromHAR(ctx, entry, paused.RequestID)
	default:
		return applyFulfill(ctx, route, paused.RequestID)
	}
}

// requestPostData reassembles a paused request's body. CDP delivers it as
// base64 chunks, and match:["body"] compares against the HAR's plain text, so
// the chunks have to be decoded and joined before either is meaningful. Chrome
// omits the entries entirely for a body it considers too long, which reads as an
// empty body and simply will not match a recorded one.
func requestPostData(req *network.Request) string {
	if req == nil || len(req.PostDataEntries) == 0 {
		return ""
	}
	var out strings.Builder
	for _, entry := range req.PostDataEntries {
		if entry == nil {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Bytes)
		if err != nil {
			return ""
		}
		out.Write(decoded)
	}
	return out.String()
}

func applyFulfill(ctx context.Context, route *Route, requestID fetch.RequestID) error {
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

// fulfillFromHAR answers a request with a recorded exchange.
func fulfillFromHAR(ctx context.Context, entry HAREntry, requestID fetch.RequestID) error {
	status := int64(entry.Status)
	if status == 0 {
		// A capture records status 0 for a request that failed or returned an
		// opaque response. Replaying that as a 0 is not expressible over CDP, and
		// 502 is the honest reading: the recording did not get an answer either.
		status = 502
	}
	headers := []*fetch.HeaderEntry{{Name: "Content-Type", Value: harContentType(entry)}}
	for name, value := range entry.Headers {
		// Content-Type comes from harContentType, which already prefers the
		// recorded one; sending the recorded header too would send it twice.
		if strings.EqualFold(name, "content-type") {
			continue
		}
		headers = append(headers, &fetch.HeaderEntry{Name: name, Value: value})
	}
	return fetch.FulfillRequest(requestID, status).
		WithResponseHeaders(headers).
		WithBody(base64.StdEncoding.EncodeToString([]byte(entry.Body))).
		Do(ctx)
}

// harContentType recovers a usable type for a recorded response.
//
// brw's own HAR export has no per-response content type to record — the in-page
// capture never sees one — so every entry it writes is application/octet-stream.
// Replaying a JSON endpoint as octet-stream breaks any page that branches on the
// header, so an absent or placeholder type is re-derived from the URL and body
// exactly as a hand-written mock's would be.
func harContentType(entry HAREntry) string {
	recorded := strings.TrimSpace(entry.ContentType)
	if recorded != "" && !strings.EqualFold(recorded, "application/octet-stream") {
		return recorded
	}
	return guessContentType(entry.URL, entry.Body)
}
