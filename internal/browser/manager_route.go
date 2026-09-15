package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

// MaxRoutesPerTab bounds the route table so a runaway caller cannot make every
// request walk an unbounded list. Exported because the extension-bridge
// transport keeps its own table and has to bound it the same way.
const MaxRoutesPerTab = 50

// maxObservedRouteMissReasons bounds what brw_observe repeats from a fixture's
// miss list: enough to name the endpoint a page is stuck on, not the whole list
// that brw_route{action:"list"} already carries.
const maxObservedRouteMissReasons = 3

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
//
// resource is the kind of request Chrome paused. A replay route declines
// anything a brw HAR cannot hold and lets the scan continue, so the rule is not
// counted as matched and a times budget is not spent on a document request the
// fixture was never going to answer.
func (r *routeState) match(tabID, url string, resource network.ResourceType) *Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	routes := r.routes[tabID]
	for i, route := range routes {
		if !matchURLGlob(route.Pattern, url) {
			continue
		}
		if route.Behaviour == RouteReplay && !harReplayableResourceType(resource) {
			if route.har != nil {
				route.har.recordNotReplayable()
			}
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

// forget drops every rule for a tab. A replay route holds its whole decoded HAR
// — up to the replay size limit in entries and bodies — so a table left behind
// after the tab closed pins the recording for the life of the daemon.
func (r *routeState) forget(tabID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, tabID)
}

// missSummary reports the tab's HAR-fixture misses and the most recent reasons,
// so brw_observe can say why a routed page is half-loaded.
func (r *routeState) missSummary(tabID string, limit int) (int, []string) {
	r.mu.Lock()
	routes := append([]*Route(nil), r.routes[tabID]...)
	r.mu.Unlock()
	total := 0
	var reasons []string
	for _, route := range routes {
		if route.har == nil {
			continue
		}
		missed, latest := route.har.missSummary(limit - len(reasons))
		total += missed
		reasons = append(reasons, latest...)
	}
	return total, reasons
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
		return RouteResult{
			Action: "replay", TabID: tabID, Routes: routes, Count: len(routes), Note: harReplayNote(route.har),
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

// CheckRouteReplay implements browser.RouteReplayer. Direct CDP pauses requests
// with the Fetch domain and writes the response itself, so a recorded exchange
// can be served.
func (m *Manager) CheckRouteReplay() error { return nil }

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
	// Asked before the switch below, so a behaviour brw refuses on purpose is
	// named as refused rather than reported as a misspelling of one it supports.
	if err := CheckRouteBehaviourSupported(opts.Behaviour); err != nil {
		return nil, err
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
// replayed ("*/api/*"). The default "*" narrows to the same thing in practice,
// because a replay only ever answers the request kinds a brw HAR can hold (see
// harReplayableResourceType) and the document and its assets load normally
// whatever the pattern says.
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
	if err := checkHARMatchIsSatisfiable(artifactID, opts.HAR, match); err != nil {
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

// checkHARMatchIsSatisfiable refuses a match key the recording cannot answer on.
//
// A request body reaches the recording through two lossy steps, and "body" as a
// match key survives neither. An export redacts request bodies unless it was
// taken with redaction:"none", so every entry of an ordinary capture records the
// body as the placeholder; and the in-page capture clips a body at
// snapshot.BodyCapBytes whatever the redaction setting, so an entry over the cap
// records a prefix the live request's whole body can never equal. Keyed on
// "body", such a fixture matches nothing: under the default
// on_miss:"passthrough" every request would go to the real backend while the
// caller believed it was mocked.
//
// Both are refused rather than counted, because a fixture that answers none of
// the requests it was installed for is a broken fixture, not a partial one.
func checkHARMatchIsSatisfiable(artifactID string, entries []HAREntry, match []string) error {
	if !slices.Contains(match, HARMatchBody) {
		return nil
	}
	redacted, clipped := 0, 0
	for _, entry := range entries {
		switch {
		case entry.RequestBody == HARRedactedPlaceholder:
			redacted++
		// The marker is checked as well as the flag, not instead of it: the flag
		// is set by whoever decoded the HAR, and a guard that a caller can slip
		// past by building an entry without it is not a guard. An external HAR
		// that declared a larger bodySize has only the flag, so both are needed.
		case entry.RequestBodyTruncated || strings.HasSuffix(entry.RequestBody, snapshot.BodyTruncationMarker):
			clipped++
		}
	}
	if redacted > 0 {
		return fmt.Errorf("match includes %q but %d of %d entries in HAR artifact %s record the request body as %q, so no request can ever match: recapture with brw_artifact_capture{kind:\"har\", redaction:\"none\"}, or drop %q from match",
			HARMatchBody, redacted, len(entries), artifactID, HARRedactedPlaceholder, HARMatchBody)
	}
	if clipped > 0 {
		return fmt.Errorf("match includes %q but %d of %d entries in HAR artifact %s record only the first %d characters of the request body (the capture clips at that cap and appends %q), so a request sending the whole body can never match: drop %q from match, or record against requests whose bodies fit under the cap",
			HARMatchBody, clipped, len(entries), artifactID, snapshot.BodyCapBytes, snapshot.BodyTruncationMarker, HARMatchBody)
	}
	return nil
}

// harReplayNote describes the installed fixture, including the two ways it
// answers less than a caller reading "replay the whole recording" would expect:
// brw's HAR holds fetch/XHR only, and its response bodies are capture snippets.
func harReplayNote(fixture *harFixture) string {
	view := fixture.view()
	note := fmt.Sprintf("fetch and XHR requests matching this pattern are answered from the %d recorded entries in %s; the document and its scripts, stylesheets and images always load from the network, because brw records a HAR from the in-page fetch/XHR wrappers and never captures them",
		view.Entries, view.ArtifactID)
	if fixture.onMiss == HARMissFail {
		note += ". A fetch or XHR the HAR does not hold is refused and reported in fixture.misses"
	} else {
		note += ". A fetch or XHR the HAR does not hold goes to the real network"
	}
	if view.Truncated > 0 {
		note += fmt.Sprintf(". %d of %d recorded responses are truncated capture snippets rather than whole bodies, so a page parsing one will see it cut short; fixture.served_truncated counts the ones actually replayed",
			view.Truncated, view.Entries)
	}
	return note
}

// harReplayableResourceType reports whether a paused request is one a brw HAR
// could have recorded.
//
// The recording comes from the in-page fetch/XHR wrappers
// (internal/snapshot/network.go), so it never holds the navigation, a script, a
// stylesheet, an image or a CORS preflight. A replay route leaves those to the
// network even under on_miss:"fail": refusing them would mean the documented
// default pattern "*" kills the navigation that loads the page under test, and
// the count is reported as fixture.not_replayable so the passthrough is visible.
func harReplayableResourceType(resource network.ResourceType) bool {
	switch resource {
	case network.ResourceTypeXHR, network.ResourceTypeFetch:
		return true
	case "":
		// An event brw could not classify: treated as replayable so a request
		// goes through the fixture rather than silently past it.
		return true
	default:
		return false
	}
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
		if route.har == nil {
			// Unreachable through brw_route, which refuses a replay without a
			// fixture. Guarded anyway because this runs on the Fetch.requestPaused
			// goroutine, which has no recover: a panic here takes the daemon down.
			return m.continueWithEnvironmentHeaders(ctx, tabID, paused)
		}
		body, readable := requestPostData(paused.Request)
		if !readable && slices.Contains(route.har.match, HARMatchBody) {
			// Matching an unreadable body as "" would key on the empty string and
			// could serve the recording of a request that genuinely had no body —
			// a wrong answer, which is worse than a named miss.
			route.har.recordUnreadableBody(paused.Request.Method, paused.Request.URL)
			if route.har.onMiss == HARMissFail {
				return fetch.FailRequest(paused.RequestID, network.ErrorReasonFailed).Do(ctx)
			}
			return m.continueWithEnvironmentHeaders(ctx, tabID, paused)
		}
		entry, found := route.har.find(harMethod(paused.Request.Method), paused.Request.URL, body)
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

// requestPostData reassembles a paused request's body, and reports whether it
// got the whole of it.
//
// CDP delivers the body as base64 chunks, and match:["body"] compares against
// the HAR's plain text, so the chunks have to be decoded and joined before
// either is meaningful. Chrome sets hasPostData but omits the chunks for a body
// it considers too long, and a file element carries no bytes at all; both read
// as an empty body. Returning that as a body would key the lookup on "" and
// could answer with the recording of a request that really had none, so the
// second return distinguishes "this request had no body" from "this request's
// body did not reach brw" and the caller misses by name instead.
func requestPostData(req *network.Request) (string, bool) {
	if req == nil {
		return "", true
	}
	var out strings.Builder
	for _, entry := range req.PostDataEntries {
		if entry == nil {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Bytes)
		if err != nil {
			return "", false
		}
		out.Write(decoded)
	}
	if out.Len() == 0 && req.HasPostData {
		return "", false
	}
	return out.String(), true
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
	for _, header := range entry.Headers {
		// Content-Type comes from harContentType, which already prefers the
		// recorded one; sending the recorded header too would send it twice.
		if strings.EqualFold(header.Name, "content-type") {
			continue
		}
		headers = append(headers, &fetch.HeaderEntry{Name: header.Name, Value: header.Value})
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
