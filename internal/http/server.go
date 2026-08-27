package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

type Server struct {
	manager   browser.Controller
	identity  brwidentity.Identity
	navPolicy *navpolicy.Policy
	usage     *usagelog.Recorder
	leases    *tabLeaseManager
	server    *http.Server

	// allowedHosts is the set of Host header values accepted when host
	// enforcement is on (loopback names plus the configured bind host).
	// enforceHost is true only for a loopback bind, where DNS-rebinding is the
	// threat; a non-loopback bind (Tailscale/LAN/wildcard) is the operator
	// deliberately exposing the daemon, so the Host allowlist is not gated there.
	allowedHosts map[string]bool
	enforceHost  bool
}

type snapshotRequest struct {
	Options  snapshot.SnapshotOptions
	MaxBytes int
}

func New(addr string, manager browser.Controller) *Server {
	return NewWithIdentity(addr, manager, brwidentity.Identity{})
}

func NewWithIdentity(addr string, manager browser.Controller, identity brwidentity.Identity) *Server {
	mux := http.NewServeMux()
	s := &Server{manager: manager, identity: identity, leases: newTabLeaseManager(defaultTabLeaseTTL), server: &http.Server{
		Addr: addr,
		// Bound slow-header clients (slowloris) without a blanket WriteTimeout,
		// which would truncate long-poll endpoints like wait_for.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}}
	s.allowedHosts, s.enforceHost = computeAllowedHosts(addr)
	s.routes(mux)
	// Wrap the router so every request first passes the same-machine browser
	// guard (DNS-rebinding + cross-origin CSRF). A loopback CLI/MCP client sends
	// a loopback Host and no browser Origin, so it is untouched.
	s.server.Handler = s.usageMiddleware(s.hostGuard(s.leaseMiddleware(mux)))
	return s
}

// SetUsageRecorder installs the metadata-only operational ledger. The recorder
// never sees request bodies, query values, page content, or response bodies.
func (s *Server) SetUsageRecorder(recorder *usagelog.Recorder) {
	s.usage = recorder
}

// computeAllowedHosts derives the Host allowlist and whether to enforce it from
// the daemon's bind address. The Host check defends against DNS-rebinding — a
// web page whose domain has been re-resolved to 127.0.0.1 carries its own Host
// header — which is only a threat for a LOOPBACK bind. A non-loopback bind (a
// specific Tailscale/LAN IP, a hostname, or a wildcard like ":17310") is the
// operator intentionally exposing the daemon "behind SSH/Tailscale with caller
// auth"; its legitimate Host may be a MagicDNS name or address we can't predict,
// so Host is not gated there. The cross-origin/CSRF guard still applies in all
// cases.
func computeAllowedHosts(addr string) (map[string]bool, bool) {
	allowed := map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
		"localhost": true,
	}
	host := bindHost(addr)
	enforce := isLoopbackHost(host)
	if host != "" {
		allowed[host] = true
	}
	return allowed, enforce
}

// bindHost extracts the lowercased host from a listen address, tolerating a
// bare host (no port) and stripping IPv6 brackets.
func bindHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
}

// isLoopbackHost reports whether host is a loopback name/IP. An empty host (a
// wildcard bind such as ":17310" or "0.0.0.0:17310") is NOT loopback — it
// listens on every interface — so Host enforcement is off for it.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostGuard rejects the two same-machine browser attacks the loopback control
// plane is otherwise open to: DNS-rebinding (caught by the Host allowlist) and
// cross-origin CSRF (caught by the Origin check). The daemon's POST endpoints
// are CORS "simple" requests, so no preflight fires and a visited web page could
// otherwise drive POST /api/page/evaluate (arbitrary JS in the signed-in tab) by
// side effect even though it cannot read the response. CLI/MCP clients send a
// loopback Host and no browser Origin, so they pass straight through.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.enforceHost && !s.allowedHosts[bindHost(r.Host)] {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "request rejected: Host " + r.Host + " is not an allowed brw control-plane host (DNS-rebinding guard); use 127.0.0.1 or localhost",
			})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin, r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "request rejected: cross-origin browser request to the brw control plane is not permitted (CSRF guard)",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigin reports whether a browser Origin may drive the control plane. A
// loopback origin and a same-host origin (a UI served from the daemon's own
// host, e.g. over Tailscale) are permitted; a genuinely cross-site origin is
// rejected as CSRF. An unparseable/opaque ("null") Origin is rejected — a
// non-browser client sends no Origin header at all and never reaches here.
func (s *Server) allowedOrigin(origin, reqHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	oh := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if isLoopbackHost(oh) {
		return true
	}
	return oh != "" && oh == bindHost(reqHost)
}

// SetNavigationPolicy installs the same opt-in allow/deny navigation guardrail
// the MCP surface enforces. Without it, the loopback HTTP control plane is a
// silent bypass of --allowed-domains/--blocked-domains (it shares the same
// controller as the MCP server), so the policy must be applied here too.
func (s *Server) SetNavigationPolicy(p *navpolicy.Policy) {
	s.navPolicy = p
}

// checkNavPolicy reports a policy violation for rawURL, or nil when allowed.
func (s *Server) checkNavPolicy(rawURL string) error {
	if s.navPolicy.Empty() {
		return nil
	}
	return s.navPolicy.Check(rawURL)
}

func (s *Server) prepareNavigation(rawURL string) (string, error) {
	return s.navPolicy.CheckNavigation(rawURL)
}

func (s *Server) normalizeNav(w http.ResponseWriter, rawURL string) (string, bool) {
	normalized, err := s.prepareNavigation(rawURL)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return "", false
	}
	return normalized, true
}

// denyNav writes a 403 and returns true when rawURL is not permitted by the
// navigation policy. Callers return early on true.
func (s *Server) denyNav(w http.ResponseWriter, rawURL string) bool {
	if err := s.checkNavPolicy(rawURL); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return true
	}
	return false
}

func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /api/browser/open", s.open)
	mux.HandleFunc("POST /api/browser/open_incognito", s.openIncognito)
	mux.HandleFunc("POST /api/browser/close_context", s.closeContext)
	mux.HandleFunc("GET /api/browser/tabs", s.tabs)
	mux.HandleFunc("GET /api/browser/tab_groups", s.tabGroups)
	mux.HandleFunc("POST /api/browser/focus", s.focus)
	mux.HandleFunc("POST /api/browser/close", s.closeTab)
	mux.HandleFunc("POST /api/browser/emulate_device", s.emulateDevice)
	mux.HandleFunc("GET /api/page/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/page/find", s.find)
	mux.HandleFunc("POST /api/page/find", s.find)
	mux.HandleFunc("GET /api/page/read", s.read)
	mux.HandleFunc("GET /api/page/read_data", s.readData)
	mux.HandleFunc("POST /api/page/click", s.click)
	mux.HandleFunc("POST /api/page/click_text", s.clickText)
	mux.HandleFunc("POST /api/page/navigate", s.navigate)
	mux.HandleFunc("POST /api/page/navigate_to", s.navigateTo)
	mux.HandleFunc("POST /api/page/drag", s.drag)
	mux.HandleFunc("POST /api/page/mouse_down", s.mouseDown)
	mux.HandleFunc("POST /api/page/mouse_up", s.mouseUp)
	mux.HandleFunc("POST /api/page/type", s.typeText)
	mux.HandleFunc("POST /api/page/fill", s.fill)
	mux.HandleFunc("POST /api/page/upload_file", s.uploadFile)
	mux.HandleFunc("POST /api/page/select", s.selectValue)
	mux.HandleFunc("POST /api/page/press", s.press)
	mux.HandleFunc("POST /api/page/scroll", s.scroll)
	mux.HandleFunc("POST /api/page/wait_for", s.waitFor)
	mux.HandleFunc("POST /api/page/hover", s.hover)
	mux.HandleFunc("POST /api/page/evaluate", s.evaluate)
	mux.HandleFunc("GET /api/page/network_requests", s.networkRequests)
	mux.HandleFunc("POST /api/page/network_requests", s.networkRequests)
	mux.HandleFunc("GET /api/page/network_capture", s.networkCapture)
	mux.HandleFunc("POST /api/page/network_capture", s.networkCapture)
	mux.HandleFunc("POST /api/page/replay_request", s.replayRequest)
	mux.HandleFunc("POST /api/page/execute_plan", s.executePlan)
	mux.HandleFunc("POST /api/page/batch", s.executeBatch)
	mux.HandleFunc("POST /api/page/cancel", s.cancel)
	mux.HandleFunc("GET /api/page/observe", s.observe)
	mux.HandleFunc("POST /api/page/commit", s.commitField)
	mux.HandleFunc("POST /api/page/notify", s.notify)
	mux.HandleFunc("POST /api/page/assert_visible", s.assertVisible)
	mux.HandleFunc("POST /api/page/assert_hidden", s.assertHidden)
	mux.HandleFunc("POST /api/page/assert_text", s.assertText)
	mux.HandleFunc("POST /api/page/assert_value", s.assertValue)
	mux.HandleFunc("POST /api/page/click_xy", s.clickXY)
	mux.HandleFunc("GET /api/page/window_bounds", s.windowBounds)
	mux.HandleFunc("POST /api/browser/resize_window", s.resizeWindow)
	mux.HandleFunc("GET /api/page/console", s.consoleMessages)
	mux.HandleFunc("GET /api/page/downloads", s.downloads)
	mux.HandleFunc("GET /api/page/trace", s.trace)
	mux.HandleFunc("POST /api/page/clear_trace", s.clearTrace)
	mux.HandleFunc("POST /api/browser/group_tabs", s.groupTabs)
	mux.HandleFunc("POST /api/browser/ungroup_tabs", s.ungroupTabs)
	mux.HandleFunc("GET /api/visual/screenshot", s.screenshot)
	mux.HandleFunc("GET /api/visual/screenshot_element", s.screenshotElement)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{"ok": true, "tab_leases": s.leases.stats()}
	if !s.identity.Empty() {
		payload["identity"] = s.identity
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) requestContext(r *http.Request) context.Context {
	return s.contextWithTabID(r.Context(), r.URL.Query().Get("tab_id"))
}

// contextWithTabID pins the target tab into the context. An explicit tab_id
// always wins. When none is supplied and the controller can resolve the active
// tab (only the extension Bridge implements activeTabResolver), it is resolved
// ONCE here and pinned, so the page handler's downstream sub-calls short-circuit
// instead of re-resolving the active tab repeatedly per logical request. The
// direct-CDP Manager and HTTP proxy do not implement the capability, so they are
// unchanged. Handlers that manage tabs themselves (open/focus/close/groups/list)
// call r.Context() directly and never reach this path.
func (s *Server) contextWithTabID(ctx context.Context, tabID string) context.Context {
	if pinned := browser.TabIDFromContext(ctx); pinned != "" {
		if strings.TrimSpace(tabID) == "" || strings.TrimSpace(tabID) == pinned {
			return ctx
		}
	}
	if tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	if resolver, ok := s.manager.(activeTabResolver); ok {
		if resolved := resolver.ResolveActiveTabID(ctx); resolved != "" {
			return browser.WithImplicitTabID(ctx, resolved)
		}
	}
	return ctx
}

// contextWithExplicitTabID pins ONLY a caller-supplied tab_id and never
// auto-resolves the active tab. The batch/plan runners and cancel use it because
// they manage focus themselves: batch/plan re-pin per step after focus_tab/open
// (auto-pinning here would make retargetPinnedTab treat the pin as an explicit
// tab and suppress retargeting), and a bare cancel must stay the wildcard kill
// switch. Mirrors the MCP server excluding these tools from one-shot pinning.
func contextWithExplicitTabID(ctx context.Context, tabID string) context.Context {
	if tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

// activeTabResolver is the optional capability a Controller may implement to
// resolve the genuinely focused tab once per request (see contextWithTabID).
type activeTabResolver interface {
	ResolveActiveTabID(context.Context) string
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL        string `json:"url"`
		Group      string `json:"group"`
		GroupID    string `json:"group_id"`
		GroupColor string `json:"group_color"`
	}
	if !decode(w, r, &req) {
		return
	}
	normalizedURL, ok := s.normalizeNav(w, req.URL)
	if !ok {
		return
	}
	req.URL = normalizedURL
	var (
		result browser.OpenResult
		err    error
	)
	owner := leaseOwner(r.Context())
	daemonGrouped := false
	switch {
	case req.Group != "" || req.GroupID != "":
		result, err = s.manager.OpenInGroup(r.Context(), req.URL, browser.TabGroupOptions{
			GroupID: req.GroupID,
			Name:    req.Group,
			Color:   req.GroupColor,
		})
	case owner != "":
		// No group requested: default the new tab into this session's per-agent
		// group so agent tabs never scatter loose (or pile into one shared group)
		// across the user's tab strip.
		daemonGrouped = true
		result, err = s.openInOwnerGroup(r.Context(), req.URL, owner)
	default:
		result, err = s.manager.Open(r.Context(), req.URL)
	}
	if err == nil {
		err = s.leases.bind(owner, result.Tab.ID, true)
	}
	if err == nil && daemonGrouped {
		s.leases.noteGroup(owner, result.Tab.ID, result.Tab.GroupID)
	}
	writeResult(w, result, err)
}

func (s *Server) openIncognito(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	normalizedURL, ok := s.normalizeNav(w, req.URL)
	if !ok {
		return
	}
	req.URL = normalizedURL
	result, err := s.manager.OpenIncognito(r.Context(), req.URL)
	if err == nil {
		err = s.leases.bind(leaseOwner(r.Context()), result.Tab.ID, true)
	}
	writeResult(w, result, err)
}

func (s *Server) closeContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BrowserContextID       string `json:"context_id"`
		LegacyBrowserContextID string `json:"browser_context_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	contextID := contextIDArg(req.BrowserContextID, req.LegacyBrowserContextID)
	owner := leaseOwner(r.Context())
	var ownedTabs []string
	var newClaims []string
	if owner != "" {
		tabs, err := s.manager.ListTabs(r.Context())
		if err != nil {
			writeResult(w, browser.ActionResult{}, err)
			return
		}
		for _, tab := range tabs {
			if tab.BrowserContextID != contextID {
				continue
			}
			ownedTabs = append(ownedTabs, tab.ID)
		}
		newClaims, err = s.leases.claimAll(owner, ownedTabs)
		if err != nil {
			writeLeaseError(w, err)
			return
		}
	}
	err := s.manager.CloseContext(r.Context(), contextID)
	if err == nil {
		for _, tabID := range ownedTabs {
			s.leases.release(owner, tabID)
		}
	} else {
		for _, tabID := range newClaims {
			s.leases.release(owner, tabID)
		}
	}
	writeResult(w, browser.ActionResult{OK: err == nil}, err)
}

func (s *Server) tabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := s.manager.ListTabs(r.Context())
	if err == nil {
		tabs = s.leases.annotate(leaseOwner(r.Context()), tabs)
	}
	writeResult(w, tabs, err)
}

func (s *Server) tabGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.manager.ListTabGroups(r.Context())
	writeResult(w, groups, err)
}

func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	tabID := tabIDArg(req.TabID, req.ID)
	owner := leaseOwner(r.Context())
	release, err := s.leases.acquire(owner, tabID, true)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	defer release()
	err = s.manager.FocusTab(r.Context(), tabID)
	if err != nil && usagelog.ClassifyError(err) == "tab_lost" {
		s.leases.release(owner, tabID)
	}
	writeResult(w, browser.ActionResult{OK: err == nil, TabID: tabID}, err)
}

func (s *Server) closeTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	tabID := tabIDArg(req.TabID, req.ID)
	owner := leaseOwner(r.Context())
	release, err := s.leases.acquire(owner, tabID, false)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	defer release()
	err = s.manager.CloseTab(r.Context(), tabID)
	if err == nil || usagelog.ClassifyError(err) == "tab_lost" {
		s.leases.release(owner, tabID)
	}
	writeResult(w, browser.ActionResult{OK: err == nil, TabID: tabID}, err)
}

func (s *Server) emulateDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		browser.DeviceEmulationOptions
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.EmulateDevice(s.contextWithTabID(r.Context(), req.TabID), req.DeviceEmulationOptions)
	writeResult(w, result, err)
}

// tabIDArg accepts either the legacy `id` field or the `tab_id` field used by
// every other page tool, preferring `tab_id` for consistency. Mirrors the MCP
// server's alias handling so callers get identical behaviour on both surfaces.
func tabIDArg(tabID, id string) string {
	if strings.TrimSpace(tabID) != "" {
		return tabID
	}
	return id
}

func contextIDArg(contextID, legacyBrowserContextID string) string {
	if strings.TrimSpace(contextID) != "" {
		return contextID
	}
	return legacyBrowserContextID
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	req, ok := parseSnapshotOptions(w, r)
	if !ok {
		return
	}
	snap, err := s.manager.Snapshot(s.requestContext(r), req.Options)
	if err == nil && req.MaxBytes > 0 {
		snap = trimSnapshotToMaxBytes(snap, req.MaxBytes)
	}
	writeResult(w, snap, err)
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) {
	opts, ok := parseFindOptions(w, r)
	if !ok {
		return
	}
	result, err := s.manager.Find(s.requestContext(r), opts)
	writeResult(w, result, err)
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	opts, ok := parseReadOptions(w, r)
	if !ok {
		return
	}
	read, err := s.manager.Read(s.requestContext(r))
	if err != nil {
		writeResult(w, read, err)
		return
	}
	// Window falls back to the whole document when a section cannot be resolved,
	// so an unresolvable section has to be rejected here. Without this the
	// endpoint answered ?section=NoSuchHeading with 200 and the entire page.
	if opts.Section != "" {
		if !readability.SectionsAddressable(read.Headings) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "section addressing is unavailable on this backend (the page read carried no heading offsets); page with offset/max_chars instead",
			})
			return
		}
		if _, ok := readability.FindSectionSpan(read.Headings, len([]rune(read.Main)), opts.Section); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":              "no section matching " + strconv.Quote(opts.Section),
				"available_sections": readability.SectionNames(read.Headings),
			})
			return
		}
	}
	writeResult(w, readability.Window(read, opts), nil)
}

// parseReadOptions reads the page-read bounds from the query string. The bounds
// mirror the brw_read tool so an HTTP client and an MCP client page a long
// document the same way.
func parseReadOptions(w http.ResponseWriter, r *http.Request) (readability.ReadOptions, bool) {
	q := r.URL.Query()
	opts := readability.ReadOptions{}

	bounded := false
	for _, field := range []struct {
		name string
		dst  *int
	}{
		{"max_chars", &opts.MaxChars},
		{"offset", &opts.Offset},
		{"max_links", &opts.MaxLinks},
		{"max_headings", &opts.MaxHeadings},
	} {
		raw := q.Get(field.name)
		value, ok := parseBoundParam(w, raw, field.name)
		if !ok {
			return opts, false
		}
		if raw != "" {
			bounded = true
		}
		*field.dst = value
	}

	// This endpoint bounds a read only when asked to. Bounding belongs at the
	// model boundary — the MCP layer applies its own window to whatever comes
	// back — and defaulting here silently truncated any client written against
	// the older unbounded contract, including an older brw proxy, which then had
	// no way to ask for the remainder.
	if !bounded {
		opts.MaxChars = readability.UnboundedReadChars
		opts.MaxLinks = readability.UnboundedReadChars
		opts.MaxHeadings = readability.UnboundedReadChars
	}

	if raw := q.Get("include"); raw != "" {
		opts.Include = readability.NormalizeSections(strings.Split(raw, ","))
	}
	opts.Section = strings.TrimSpace(q.Get("section"))
	if err := opts.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return opts, false
	}
	return opts, true
}

// parseBoundParam accepts -1 as the explicit "no cap" sentinel, which
// parseIntParam rejects along with genuinely negative values.
func parseBoundParam(w http.ResponseWriter, raw, name string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < readability.UnboundedReadChars {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": name + " must be a non-negative integer, or -1 for no cap"})
		return 0, false
	}
	return value, true
}

func (s *Server) readData(w http.ResponseWriter, r *http.Request) {
	data, err := s.manager.ReadData(s.requestContext(r))
	writeResult(w, data, err)
}

func (s *Server) click(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref        string   `json:"ref"`
		X          *float64 `json:"x"`
		Y          *float64 `json:"y"`
		Button     string   `json:"button"`
		ClickCount int      `json:"click_count"`
		Snapshot   bool     `json:"snapshot"`
		TabID      string   `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	if browser.IsDefaultLeftSingleRefClick(req.Button, req.ClickCount, req.Ref, req.X, req.Y) {
		result, err := s.manager.Click(ctx, req.Ref)
		s.writeActionResult(w, r, result, err)
		return
	}
	result, err := s.manager.ClickButton(ctx, browser.ClickButtonOptions{
		MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
		Button:     req.Button,
		ClickCount: req.ClickCount,
	})
	s.writeActionResult(w, r, result, err)
}

func (s *Server) drag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From   browser.MousePoint `json:"from"`
		To     browser.MousePoint `json:"to"`
		Steps  int                `json:"steps"`
		Button string             `json:"button"`
		TabID  string             `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	opts := browser.DragOptions{
		From:   req.From,
		To:     req.To,
		Steps:  req.Steps,
		Button: req.Button,
	}
	if err := opts.Validate(); err != nil {
		writeResult(w, browser.ActionResult{}, err)
		return
	}
	result, err := s.manager.Drag(s.contextWithTabID(r.Context(), req.TabID), opts)
	writeResult(w, result, err)
}

func (s *Server) mouseDown(w http.ResponseWriter, r *http.Request) {
	opts, tabID, ok := decodeMouseButton(w, r)
	if !ok {
		return
	}
	result, err := s.manager.MouseDown(s.contextWithTabID(r.Context(), tabID), opts)
	writeResult(w, result, err)
}

func (s *Server) mouseUp(w http.ResponseWriter, r *http.Request) {
	opts, tabID, ok := decodeMouseButton(w, r)
	if !ok {
		return
	}
	result, err := s.manager.MouseUp(s.contextWithTabID(r.Context(), tabID), opts)
	writeResult(w, result, err)
}

func decodeMouseButton(w http.ResponseWriter, r *http.Request) (browser.MouseButtonOptions, string, bool) {
	var req struct {
		Ref    string   `json:"ref"`
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
		Button string   `json:"button"`
		TabID  string   `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return browser.MouseButtonOptions{}, "", false
	}
	return browser.MouseButtonOptions{
		MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
		Button:     req.Button,
	}, req.TabID, true
}

func (s *Server) clickText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		snapshot.ClickTextOptions
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.ClickText(ctx, req.ClickTextOptions)
	s.writeActionResult(w, r, result, err)
}

func (s *Server) writeActionResult(w http.ResponseWriter, r *http.Request, result browser.ActionResult, err error) {
	if err == nil && result.NewTabID != "" {
		err = s.leases.bind(leaseOwner(r.Context()), result.NewTabID, true)
	}
	if err != nil {
		if _, ok := err.(*tabLeaseConflictError); ok {
			writeLeaseError(w, err)
			return
		}
	}
	writeResult(w, result, err)
}

func (s *Server) navigate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
		Snapshot  bool   `json:"snapshot"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Navigate(ctx, req.Direction)
	writeResult(w, result, err)
}

func (s *Server) navigateTo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	normalizedURL, ok := s.normalizeNav(w, req.URL)
	if !ok {
		return
	}
	req.URL = normalizedURL
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.NavigateTo(ctx, req.URL)
	writeResult(w, result, err)
}

func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref      string `json:"ref"`
		Text     string `json:"text"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Type(ctx, req.Ref, req.Text)
	writeResult(w, result, err)
}

func (s *Server) fill(w http.ResponseWriter, r *http.Request) {
	req := struct {
		snapshot.FillOptions
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}{FillOptions: snapshot.FillOptions{Replace: true}}
	if !decode(w, r, &req) {
		return
	}
	// Playwright-style value alias → text (same contract as the MCP surface).
	req.Text = req.EffectiveText()
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Fill(ctx, req.FillOptions)
	writeResult(w, result, err)
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		snapshot.UploadOptions
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.URL != "" && s.denyNav(w, req.URL) {
		return
	}
	result, err := s.manager.UploadFile(s.contextWithTabID(r.Context(), req.TabID), req.UploadOptions)
	writeResult(w, result, err)
}

func (s *Server) selectValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref      string `json:"ref"`
		Value    string `json:"value"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Select(ctx, req.Ref, req.Value)
	writeResult(w, result, err)
}

func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key      string `json:"key"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Press(ctx, req.Key)
	writeResult(w, result, err)
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
		Snapshot  bool   `json:"snapshot"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Scroll(ctx, req.Direction)
	writeResult(w, result, err)
}

func (s *Server) waitFor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Condition string `json:"condition"`
		TimeoutMS int    `json:"timeout_ms"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.WaitFor(s.contextWithTabID(r.Context(), req.TabID), req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) hover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref      string `json:"ref"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := s.manager.Hover(ctx, req.Ref)
	writeResult(w, result, err)
}

func (s *Server) evaluate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expression string `json:"expression"`
		TabID      string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Evaluate(s.contextWithTabID(r.Context(), req.TabID), req.Expression)
	writeResult(w, result, err)
}

func (s *Server) networkRequests(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	ctx := s.requestContext(r)
	if r.Method == http.MethodPost {
		var req struct {
			Filter string `json:"filter"`
			TabID  string `json:"tab_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		filter = req.Filter
		ctx = s.contextWithTabID(r.Context(), req.TabID)
	}
	result, err := s.manager.NetworkRequests(ctx, filter)
	writeResult(w, result, err)
}

func (s *Server) networkCapture(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	ctx := s.requestContext(r)
	if r.Method == http.MethodPost {
		var req struct {
			Filter string `json:"filter"`
			TabID  string `json:"tab_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		filter = req.Filter
		ctx = s.contextWithTabID(r.Context(), req.TabID)
	}
	result, err := s.manager.NetworkCapture(ctx, filter)
	writeResult(w, result, err)
}

func (s *Server) replayRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method   string            `json:"method"`
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		Body     string            `json:"body"`
		Offset   int               `json:"offset"`
		MaxBytes int               `json:"max_bytes"`
		TabID    string            `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.denyNav(w, req.URL) {
		return
	}
	result, err := s.manager.ReplayRequest(s.contextWithTabID(r.Context(), req.TabID), browser.ReplayRequestParams{
		Method:   req.Method,
		URL:      req.URL,
		Headers:  req.Headers,
		Body:     req.Body,
		Offset:   req.Offset,
		MaxBytes: req.MaxBytes,
	})
	writeResult(w, result, err)
}

func (s *Server) executePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Steps []browser.PlanStep `json:"steps"`
		TabID string             `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	for i := range req.Steps {
		if !strings.EqualFold(req.Steps[i].Action, "open") || req.Steps[i].URL == "" {
			continue
		}
		normalizedURL, ok := s.normalizeNav(w, req.Steps[i].URL)
		if !ok {
			return
		}
		req.Steps[i].URL = normalizedURL
	}
	owner := leaseOwner(r.Context())
	var focusTabs []string
	for _, step := range req.Steps {
		if owner != "" && strings.EqualFold(step.Action, "focus_tab") {
			focusTabs = append(focusTabs, step.ID)
		}
	}
	reservedTabs, err := s.leases.claimAll(owner, focusTabs)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	result, err := s.manager.ExecutePlan(contextWithExplicitTabID(r.Context(), req.TabID), req.Steps)
	usedTabs := make(map[string]bool)
	if err == nil {
		for _, stepResult := range result.Steps {
			if stepResult.Index < 0 || stepResult.Index >= len(req.Steps) {
				continue
			}
			step := req.Steps[stepResult.Index]
			if !stepResult.OK {
				if strings.EqualFold(step.Action, "focus_tab") && usagelog.ClassifyError(errors.New(stepResult.Error)) == "tab_lost" {
					s.leases.release(owner, step.ID)
				}
				continue
			}
			if newTabID := planActionNewTabID(stepResult.Result); newTabID != "" {
				err = s.leases.bind(owner, newTabID, true)
				if err != nil {
					break
				}
			}
			switch {
			case strings.EqualFold(step.Action, "focus_tab"):
				err = s.leases.bind(owner, step.ID, true)
				usedTabs[step.ID] = true
			case strings.EqualFold(step.Action, "open"):
				err = s.leases.bind(owner, planOpenTabID(stepResult.Result), true)
			}
			if err != nil {
				break
			}
		}
	}
	for _, tabID := range reservedTabs {
		if !usedTabs[tabID] {
			s.leases.release(owner, tabID)
		}
	}
	if _, ok := err.(*tabLeaseConflictError); ok {
		writeLeaseError(w, err)
		return
	}
	writeResult(w, result, err)
}

func (s *Server) executeBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Steps []browser.BatchStep `json:"steps"`
		TabID string              `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	for i := range req.Steps {
		if !strings.EqualFold(req.Steps[i].Action, "open") || req.Steps[i].URL == "" {
			continue
		}
		normalizedURL, ok := s.normalizeNav(w, req.Steps[i].URL)
		if !ok {
			return
		}
		req.Steps[i].URL = normalizedURL
	}
	owner := leaseOwner(r.Context())
	var focusTabs []string
	for _, step := range req.Steps {
		if owner != "" && strings.EqualFold(step.Action, "focus_tab") {
			focusTabs = append(focusTabs, step.ID)
		}
	}
	reservedTabs, err := s.leases.claimAll(owner, focusTabs)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	result, err := s.manager.ExecuteBatch(contextWithExplicitTabID(r.Context(), req.TabID), req.Steps)
	usedTabs := make(map[string]bool)
	if err == nil {
		for _, step := range result.Steps {
			if !step.OK {
				if step.Index >= 0 && step.Index < len(req.Steps) && strings.EqualFold(step.Action, "focus_tab") && usagelog.ClassifyError(errors.New(step.Error)) == "tab_lost" {
					s.leases.release(owner, req.Steps[step.Index].ID)
				}
				continue
			}
			if step.NewTabID != "" {
				err = s.leases.bind(owner, step.NewTabID, true)
			} else if strings.EqualFold(step.Action, "focus_tab") && step.TabID != "" {
				err = s.leases.bind(owner, step.TabID, true)
				usedTabs[step.TabID] = true
			}
			if err != nil {
				break
			}
		}
	}
	for _, tabID := range reservedTabs {
		if !usedTabs[tabID] {
			s.leases.release(owner, tabID)
		}
	}
	if _, ok := err.(*tabLeaseConflictError); ok {
		writeLeaseError(w, err)
		return
	}
	writeResult(w, result, err)
}

func planOpenTabID(value any) string {
	if result, ok := value.(browser.OpenResult); ok {
		return result.Tab.ID
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var result browser.OpenResult
	if json.Unmarshal(data, &result) != nil {
		return ""
	}
	return result.Tab.ID
}

func planActionNewTabID(value any) string {
	if result, ok := value.(browser.ActionResult); ok {
		return result.NewTabID
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var result browser.ActionResult
	if json.Unmarshal(data, &result) != nil {
		return ""
	}
	return result.NewTabID
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	// A bare cancel (no tab_id) must stay the wildcard kill switch: only pin when
	// the caller supplied an explicit tab_id, never auto-resolve the active tab
	// (that would scope the cancel to one tab).
	result, err := s.manager.Cancel(contextWithExplicitTabID(r.Context(), req.TabID), req.Token)
	writeResult(w, result, err)
}

func (s *Server) observe(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.Observe(s.requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) commitField(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CommitField(s.contextWithTabID(r.Context(), req.TabID), req.Ref))
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Message string `json:"message"`
		TabID   string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Notify(s.contextWithTabID(r.Context(), req.TabID), browser.NotifyOptions{Kind: req.Kind, Title: req.Title, Message: req.Message})
	writeResult(w, result, err)
}

func (s *Server) assertVisible(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref       string `json:"ref"`
		TimeoutMS int    `json:"timeout_ms"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertVisible(s.contextWithTabID(r.Context(), req.TabID), req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) assertHidden(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref       string `json:"ref"`
		TimeoutMS int    `json:"timeout_ms"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertHidden(s.contextWithTabID(r.Context(), req.TabID), req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) assertText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref       string `json:"ref"`
		Text      string `json:"text"`
		TimeoutMS int    `json:"timeout_ms"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertText(s.contextWithTabID(r.Context(), req.TabID), req.Ref, req.Text, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) assertValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref       string `json:"ref"`
		Value     string `json:"value"`
		TimeoutMS int    `json:"timeout_ms"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertValue(s.contextWithTabID(r.Context(), req.TabID), req.Ref, req.Value, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) clickXY(w http.ResponseWriter, r *http.Request) {
	var req struct {
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
		TabID string  `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.ClickXY(s.contextWithTabID(r.Context(), req.TabID), req.X, req.Y)
	writeResult(w, result, err)
}

func (s *Server) windowBounds(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.WindowBounds(s.requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) resizeWindow(w http.ResponseWriter, r *http.Request) {
	// tab_id is transport routing rather than a resize parameter, so it is
	// decoded alongside the options instead of living on the options type.
	var req struct {
		browser.WindowResizeOptions
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	result, err := s.manager.ResizeWindow(ctx, req.WindowResizeOptions)
	writeResult(w, result, err)
}

func (s *Server) consoleMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := parseBoundParam(w, q.Get("limit"), "limit")
	if !ok {
		return
	}
	// The console drains on read, so a client that asked for no limit must get
	// everything: capping it would discard messages it can never fetch again.
	// Only a caller that named a filter opts into the default cap.
	if q.Get("limit") == "" && q.Get("only_errors") == "" && q.Get("level") == "" && q.Get("pattern") == "" {
		limit = -1
	}
	var match *regexp.Regexp
	if pattern := q.Get("pattern"); pattern != "" {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid pattern: " + err.Error()})
			return
		}
		match = compiled
	}

	messages, err := s.manager.ConsoleMessages(s.requestContext(r))
	if err != nil {
		writeResult(w, messages, err)
		return
	}
	writeResult(w, filterConsoleMessages(messages, q.Get("only_errors") == "true", q.Get("level"), match, limit), nil)
}

// defaultHTTPConsoleLimit mirrors the cap the brw_console tool applies.
const defaultHTTPConsoleLimit = 100

// filterConsoleMessages narrows a console read the same way brw_console does,
// and returns a bare slice: the --upstream-http proxy decodes this endpoint
// into []ConsoleMessage, so an added response envelope would silently empty the
// console in proxy mode. This endpoint has no retention buffer behind it —
// each request filters only what its own drain returned — so a caller that
// needs to keep unmatched messages should read unfiltered.
func filterConsoleMessages(messages []browser.ConsoleMessage, onlyErrors bool, level string, match *regexp.Regexp, limit int) []browser.ConsoleMessage {
	kept := make([]browser.ConsoleMessage, 0, len(messages))
	for _, msg := range messages {
		if onlyErrors && !isErrorConsoleLevel(msg.Level) {
			continue
		}
		if level != "" && !strings.EqualFold(level, msg.Level) {
			continue
		}
		if match != nil && !match.MatchString(msg.Text) {
			continue
		}
		kept = append(kept, msg)
	}
	if limit == 0 {
		limit = defaultHTTPConsoleLimit
	}
	if limit > 0 && len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	return kept
}

func isErrorConsoleLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "assert", "exception", "severe":
		return true
	default:
		return false
	}
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.Downloads(s.requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.scopedTrace(r))
}

// scopedTrace returns only the actions this caller is entitled to see. The
// daemon is shared, so an unscoped trace hands one agent session another's
// browsing — on a signed-in profile that means authenticated URLs. Entries for a
// tab leased by somebody else are withheld and counted, so a caller can tell a
// filtered trace from an empty one.
func (s *Server) scopedTrace(r *http.Request) browser.TraceResult {
	full := s.manager.GetTrace()
	owner := leaseOwner(r.Context())
	if owner == "" {
		// No lease identity on this request: return only tab-less entries rather
		// than everything, since there is no way to establish entitlement.
		return filterTrace(full, func(entry browser.TraceEntry) bool {
			return entry.TabID == ""
		})
	}
	return filterTrace(full, func(entry browser.TraceEntry) bool {
		return entry.TabID == "" || s.leases.ownsTab(owner, entry.TabID)
	})
}

func filterTrace(full browser.TraceResult, keep func(browser.TraceEntry) bool) browser.TraceResult {
	out := browser.TraceResult{Entries: make([]browser.TraceEntry, 0, len(full.Entries))}
	for _, entry := range full.Entries {
		if keep(entry) {
			out.Entries = append(out.Entries, entry)
		}
	}
	out.Count = len(out.Entries)
	out.Withheld = len(full.Entries) - len(out.Entries)
	return out
}

func (s *Server) clearTrace(w http.ResponseWriter, _ *http.Request) {
	s.manager.ClearTrace()
	writeJSON(w, http.StatusOK, browser.ActionResult{OK: true})
}

func (s *Server) groupTabs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabIDs  []string `json:"tab_ids"`
		Name    string   `json:"name"`
		Color   string   `json:"color"`
		GroupID string   `json:"group_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	owner := leaseOwner(r.Context())
	newClaims, err := s.leases.claimAll(owner, req.TabIDs)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	err = s.manager.GroupTabs(r.Context(), req.TabIDs, browser.TabGroupOptions{
		GroupID: req.GroupID,
		Name:    req.Name,
		Color:   req.Color,
	})
	if err != nil {
		for _, tabID := range newClaims {
			s.leases.release(owner, tabID)
		}
	}
	writeResult(w, browser.ActionResult{OK: err == nil}, err)
}

func (s *Server) ungroupTabs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabIDs []string `json:"tab_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	owner := leaseOwner(r.Context())
	newClaims, err := s.leases.claimAll(owner, req.TabIDs)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	err = s.manager.UngroupTabs(r.Context(), req.TabIDs)
	if err != nil {
		for _, tabID := range newClaims {
			s.leases.release(owner, tabID)
		}
	}
	writeResult(w, browser.ActionResult{OK: err == nil}, err)
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Set-of-Marks capture: draw ref-labelled boxes over frontier elements and
	// return the PNG plus a ref->box legend. The legend is only representable in
	// the JSON (base64) response; a raw response still returns the annotated PNG
	// bytes but drops the legend.
	// A ref or region query implies an annotated (Set-of-Marks) crop even without
	// annotate=1 — the legend is the point of the crop.
	ref := q.Get("ref")
	region, hasRegion := parseScreenshotRegion(q)
	if q.Get("annotate") == "1" || strings.TrimSpace(ref) != "" || hasRegion {
		aopts := browser.AnnotatedScreenshotOptions{Mode: "frontier", Ref: ref}
		if hasRegion {
			aopts.Region = region
		}
		shot, err := s.manager.ScreenshotAnnotated(s.requestContext(r), aopts)
		if err != nil {
			writeError(w, err)
			return
		}
		if q.Get("base64") == "1" {
			writeJSON(w, http.StatusOK, shot)
			return
		}
		w.Header().Set("content-type", shot.MIMEType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(shot.Data)
		return
	}
	shot, err := s.manager.Screenshot(s.requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if q.Get("base64") == "1" {
		writeJSON(w, http.StatusOK, shot)
		return
	}
	w.Header().Set("content-type", shot.MIMEType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(shot.Data)
}

func (s *Server) screenshotElement(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	shot, err := s.manager.ScreenshotElement(s.requestContext(r), ref)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("base64") == "1" {
		writeJSON(w, http.StatusOK, shot)
		return
	}
	w.Header().Set("content-type", shot.MIMEType)
	w.Header().Set("content-length", strconv.Itoa(len(shot.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(shot.Data)
}

// parseScreenshotRegion reads an optional viewport-space clip rectangle from the
// region_x/region_y/region_w/region_h query params for a tight annotated crop.
// Returns ok=false (and a zero region) when no usable width/height is supplied.
func parseScreenshotRegion(q url.Values) (browser.ScreenshotRegion, bool) {
	parse := func(k string) float64 {
		v, _ := strconv.ParseFloat(q.Get(k), 64)
		return v
	}
	region := browser.ScreenshotRegion{
		X:      parse("region_x"),
		Y:      parse("region_y"),
		Width:  parse("region_w"),
		Height: parse("region_h"),
	}
	if region.IsZero() {
		return browser.ScreenshotRegion{}, false
	}
	return region, true
}

func parseSnapshotOptions(w http.ResponseWriter, r *http.Request) (snapshotRequest, bool) {
	q := r.URL.Query()
	viewportOnly, ok := parseBoolValue(w, q.Get("viewport_only"), "viewport_only")
	if !ok {
		return snapshotRequest{}, false
	}
	includeAX, ok := parseBoolValue(w, q.Get("include_ax"), "include_ax")
	if !ok {
		return snapshotRequest{}, false
	}
	includeHidden, ok := parseBoolValue(w, q.Get("include_hidden"), "include_hidden")
	if !ok {
		return snapshotRequest{}, false
	}
	includeFrames, ok := parseBoolValue(w, q.Get("include_frames"), "include_frames")
	if !ok {
		return snapshotRequest{}, false
	}
	textContent, ok := parseBoolValue(w, q.Get("text_content"), "text_content")
	if !ok {
		return snapshotRequest{}, false
	}
	visualIslands, ok := parseBoolValue(w, q.Get("visual_islands"), "visual_islands")
	if !ok {
		return snapshotRequest{}, false
	}
	limit, ok := parseIntParam(w, q.Get("limit"), "limit")
	if !ok {
		return snapshotRequest{}, false
	}
	visualIslandsLimit, ok := parseIntParam(w, q.Get("visual_islands_limit"), "visual_islands_limit")
	if !ok {
		return snapshotRequest{}, false
	}
	since, ok := parseInt64Param(w, q.Get("since"), "since")
	if !ok {
		return snapshotRequest{}, false
	}
	maxBytes, ok := parseIntParam(w, q.Get("max_bytes"), "max_bytes")
	if !ok {
		return snapshotRequest{}, false
	}
	return snapshotRequest{
		// Share the MCP surface's default envelope: an unspecified mode collapses
		// to the bounded frontier so HTTP callers don't get unbounded multi-thousand
		// element dumps on dense pages.
		Options: snapshot.NormalizeOptions(snapshot.SnapshotOptions{
			Mode:               q.Get("mode"),
			Query:              q.Get("query"),
			Role:               q.Get("role"),
			Text:               q.Get("text"),
			Limit:              limit,
			ViewportOnly:       viewportOnly,
			IncludeHidden:      includeHidden,
			IncludeAX:          includeAX,
			IncludeFrames:      includeFrames,
			TextContent:        textContent,
			VisualIslands:      visualIslands,
			VisualIslandsLimit: visualIslandsLimit,
			Since:              since,
		}),
		MaxBytes: maxBytes,
	}, true
}

func parseFindOptions(w http.ResponseWriter, r *http.Request) (snapshot.FindOptions, bool) {
	if r.Method == http.MethodPost {
		var opts snapshot.FindOptions
		if !decode(w, r, &opts) {
			return snapshot.FindOptions{}, false
		}
		if opts.Limit < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be non-negative"})
			return snapshot.FindOptions{}, false
		}
		return opts, true
	}
	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit")
	if !ok {
		return snapshot.FindOptions{}, false
	}
	viewportOnly, ok := parseBoolValue(w, q.Get("viewport_only"), "viewport_only")
	if !ok {
		return snapshot.FindOptions{}, false
	}
	includeHidden, ok := parseBoolValue(w, q.Get("include_hidden"), "include_hidden")
	if !ok {
		return snapshot.FindOptions{}, false
	}
	textContent, ok := parseBoolValue(w, q.Get("text_content"), "text_content")
	if !ok {
		return snapshot.FindOptions{}, false
	}
	return snapshot.FindOptions{
		Query:         q.Get("query"),
		Role:          q.Get("role"),
		Text:          q.Get("text"),
		Limit:         limit,
		ViewportOnly:  viewportOnly,
		IncludeHidden: includeHidden,
		TextContent:   textContent,
	}, true
}

func parseBoolValue(w http.ResponseWriter, raw, name string) (bool, bool) {
	if raw == "" {
		return false, true
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": name + " must be a boolean"})
		return false, false
	}
	return value, true
}

func parseIntParam(w http.ResponseWriter, raw, name string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": name + " must be a non-negative integer"})
		return 0, false
	}
	return value, true
}

func trimSnapshotToMaxBytes(snap snapshot.PageSnapshot, maxBytes int) snapshot.PageSnapshot {
	for len(snap.Elements) > 0 {
		data, err := json.Marshal(snap)
		if err != nil || len(data) <= maxBytes {
			return snap
		}
		snap.Elements = snap.Elements[:len(snap.Elements)-1]
	}
	return snap
}

func parseInt64Param(w http.ResponseWriter, raw, name string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": name + " must be a non-negative integer"})
		return 0, false
	}
	return value, true
}

// maxRequestBodyBytes caps decoded request bodies so a single oversized payload
// (forwarded into the browser by several endpoints) can't OOM the daemon.
const maxRequestBodyBytes = 8 << 20 // 8 MiB

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set(usagelog.HeaderErrorClass, usagelog.ClassifyError(err))
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(err.Error()))
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
