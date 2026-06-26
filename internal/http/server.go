package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/snapshot"
)

type Server struct {
	manager   browser.Controller
	identity  brwidentity.Identity
	navPolicy *navpolicy.Policy
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
	s := &Server{manager: manager, identity: identity, server: &http.Server{
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
	s.server.Handler = s.hostGuard(mux)
	return s
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
	payload := map[string]any{"ok": true}
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
	if tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	if resolver, ok := s.manager.(activeTabResolver); ok {
		if resolved := resolver.ResolveActiveTabID(ctx); resolved != "" {
			return browser.WithTabID(ctx, resolved)
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
	if s.denyNav(w, req.URL) {
		return
	}
	var (
		result browser.OpenResult
		err    error
	)
	if req.Group != "" || req.GroupID != "" {
		result, err = s.manager.OpenInGroup(r.Context(), req.URL, browser.TabGroupOptions{
			GroupID: req.GroupID,
			Name:    req.Group,
			Color:   req.GroupColor,
		})
	} else {
		result, err = s.manager.Open(r.Context(), req.URL)
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
	if s.denyNav(w, req.URL) {
		return
	}
	result, err := s.manager.OpenIncognito(r.Context(), req.URL)
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CloseContext(r.Context(), contextIDArg(req.BrowserContextID, req.LegacyBrowserContextID)))
}

func (s *Server) tabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := s.manager.ListTabs(r.Context())
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.FocusTab(r.Context(), tabIDArg(req.TabID, req.ID)))
}

func (s *Server) closeTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CloseTab(r.Context(), tabIDArg(req.TabID, req.ID)))
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
	read, err := s.manager.Read(s.requestContext(r))
	writeResult(w, read, err)
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
		writeResult(w, result, err)
		return
	}
	result, err := s.manager.ClickButton(ctx, browser.ClickButtonOptions{
		MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
		Button:     req.Button,
		ClickCount: req.ClickCount,
	})
	writeResult(w, result, err)
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
	result, err := s.manager.Drag(s.contextWithTabID(r.Context(), req.TabID), browser.DragOptions{
		From:   req.From,
		To:     req.To,
		Steps:  req.Steps,
		Button: req.Button,
	})
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
	if s.denyNav(w, req.URL) {
		return
	}
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
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		TabID   string            `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.denyNav(w, req.URL) {
		return
	}
	result, err := s.manager.ReplayRequest(s.contextWithTabID(r.Context(), req.TabID), browser.ReplayRequestParams{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
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
	result, err := s.manager.ExecutePlan(contextWithExplicitTabID(r.Context(), req.TabID), req.Steps)
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
	result, err := s.manager.ExecuteBatch(contextWithExplicitTabID(r.Context(), req.TabID), req.Steps)
	writeResult(w, result, err)
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

func (s *Server) consoleMessages(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.ConsoleMessages(s.requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.Downloads(s.requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) trace(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.GetTrace())
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.GroupTabs(r.Context(), req.TabIDs, browser.TabGroupOptions{
		GroupID: req.GroupID,
		Name:    req.Name,
		Color:   req.Color,
	}))
}

func (s *Server) ungroupTabs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabIDs []string `json:"tab_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.UngroupTabs(r.Context(), req.TabIDs))
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
	limit, ok := parseIntParam(w, q.Get("limit"), "limit")
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
			Mode:          q.Get("mode"),
			Query:         q.Get("query"),
			Role:          q.Get("role"),
			Text:          q.Get("text"),
			Limit:         limit,
			ViewportOnly:  viewportOnly,
			IncludeHidden: includeHidden,
			IncludeAX:     includeAX,
			IncludeFrames: includeFrames,
			Since:         since,
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
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
