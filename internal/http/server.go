package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/pagewatch"
	"github.com/Don-Works/brw/internal/plugin"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

type Server struct {
	approvalGate          *approvalgate.Gate
	approvals             *approval.Store
	approvalOperatorToken string
	pageWatch             pagewatch.API
	manager               browser.Controller
	artifacts             artifact.API
	recipes               recipe.API

	baselineRoutes recipe.BaselineRouter
	identity       brwidentity.Identity
	navPolicy      *navpolicy.Policy

	consent *siteconsent.Guard

	upstreamConsent upstreamConsentSource
	usage           *usagelog.Recorder
	leases          *tabLeaseManager

	version string

	idle    idleTracker
	server  *http.Server
	plugins *plugin.Registry

	loopbackBind bool

	allowedHosts map[string]bool
	enforceHost  bool

	roster            ProfileRoster
	profilePolicyPath string

	authDigest []byte
}

type snapshotRequest struct {
	Options  snapshot.SnapshotOptions
	MaxBytes int
}

const maxArtifactRequestBodyBytes = 4 << 10

type artifactIDRequest struct {
	ArtifactID string `json:"artifact_id"`
}

type artifactReadRequest struct {
	ArtifactID string `json:"artifact_id"`
	Offset     int64  `json:"offset"`
	MaxBytes   int    `json:"max_bytes"`
}

type artifactSearchRequest struct {
	ArtifactID string `json:"artifact_id"`
	Query      string `json:"query"`
	Limit      int    `json:"limit"`
}

func New(addr string, manager browser.Controller) *Server {
	return NewWithIdentity(addr, manager, brwidentity.Identity{})
}

func NewWithIdentity(addr string, manager browser.Controller, identity brwidentity.Identity) *Server {
	mux := http.NewServeMux()
	s := &Server{manager: manager, identity: identity, leases: newTabLeaseManager(defaultTabLeaseTTL), server: &http.Server{
		Addr: addr,

		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}}
	s.allowedHosts, s.enforceHost = computeAllowedHosts(addr)
	s.loopbackBind = isLoopbackHost(bindHost(addr))

	s.upstreamConsent, _ = manager.(upstreamConsentSource)
	s.routes(mux)

	common := func(next http.Handler) http.Handler {
		return s.usageMiddleware(s.hostGuard(s.artifactPrivacyHeaders(s.consentMiddleware(s.leaseMiddleware(s.approvalMiddleware(next))))))
	}
	s.server.Handler = s.idleMiddleware(s.approvalRoutes(s.bearerGuard(common(mux)), common))
	return s
}

// InFlight is the number of API requests currently executing against a leased tab.
func (s *Server) InFlight() int {
	inFlight, _ := s.leases.stats()["in_flight"].(int)
	return inFlight
}

// SetUsageRecorder installs the metadata-only operational ledger.
func (s *Server) SetUsageRecorder(recorder *usagelog.Recorder) {
	s.usage = recorder
}

// SetArtifactAPI installs the browser-host artifact capability.
func (s *Server) SetArtifactAPI(api artifact.API) { s.artifacts = api }

// SetRecipeAPI installs private recipe discovery and deterministic execution.
func (s *Server) SetRecipeAPI(api recipe.API) { s.recipes = api }

// SetBaselineRouter installs the baseline routing question for proxying daemons to ask.
func (s *Server) SetBaselineRouter(router recipe.BaselineRouter) { s.baselineRoutes = router }

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

func bindHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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

func (s *Server) artifactPrivacyHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/artifacts/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if isCanonicalArtifactPath(r.URL.Path) && r.Method != http.MethodPost {
				artifactPostOnly(w)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isCanonicalArtifactPath(path string) bool {
	switch path {
	case "/api/artifacts/info", "/api/artifacts/read", "/api/artifacts/search", "/api/artifacts/delete":
		return true
	default:
		return false
	}
}

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

// SetNavigationPolicy installs the same opt-in allow/deny navigation guardrail the MCP surface enforces.
func (s *Server) SetNavigationPolicy(p *navpolicy.Policy) {
	s.navPolicy = p
}

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

func (s *Server) denyNav(w http.ResponseWriter, rawURL string) bool {
	if err := s.checkNavPolicy(rawURL); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return true
	}
	return false
}

// Handler exposes the routed, middleware-wrapped handler this daemon serves, so a client in another package can be driven against the real request decoding rather than a permissive stand-in that accepts anything sent to it.
func (s *Server) Handler() http.Handler {
	return s.server.Handler
}

func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/approvals/{id}", s.approvalStatus)
	mux.HandleFunc("POST /api/watchers/register", s.watchPage)
	mux.HandleFunc("POST /api/watchers/manage", s.pageWatchers)
	mux.HandleFunc("POST /api/watchers/events", s.pageEvents)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /api/usage/report", s.reportUsage)

	mux.HandleFunc("GET /profiles", s.rosterPage)
	mux.HandleFunc("GET /profiles/roster.js", s.rosterScript)
	mux.HandleFunc("GET /api/roster/board", s.rosterBoard)
	mux.HandleFunc("POST /api/roster/profiles", s.rosterCreate)
	mux.HandleFunc("POST /api/roster/copy", s.rosterCopy)
	mux.HandleFunc("POST /api/roster/pin", s.rosterPin)
	mux.HandleFunc("POST /api/roster/open", s.rosterOpen)

	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc("GET /dashboard/stream", s.dashboardStream)
	mux.HandleFunc("GET /dashboard/activity", s.dashboardActivity)

	mux.HandleFunc("POST /dashboard/takeover", s.dashboardTakeover)
	mux.HandleFunc("POST /dashboard/input", s.dashboardInput)
	mux.HandleFunc("GET /api/session/stream", s.sessionStream)
	mux.HandleFunc("POST /api/session/release", s.releaseSession)
	mux.HandleFunc("POST /api/browser/open", s.open)
	mux.HandleFunc("POST /api/browser/open_incognito", s.openIncognito)
	mux.HandleFunc("POST /api/browser/close_context", s.closeContext)
	mux.HandleFunc("GET /api/browser/tabs", s.tabs)
	mux.HandleFunc("GET /api/browser/active_tab", s.activeTab)
	mux.HandleFunc("GET /api/browser/tab_groups", s.tabGroups)
	mux.HandleFunc("POST /api/browser/focus", s.focus)
	mux.HandleFunc("POST /api/browser/close", s.closeTab)
	mux.HandleFunc("POST /api/browser/emulate_device", s.emulateDevice)
	mux.HandleFunc("POST /api/browser/download_path", s.downloadPath)
	mux.HandleFunc("POST /api/page/geolocation", s.geolocation)
	mux.HandleFunc("POST /api/page/network_conditions", s.networkConditions)
	mux.HandleFunc("POST /api/page/emulate_media", s.emulateMedia)
	mux.HandleFunc("POST /api/page/locale", s.setLocale)
	mux.HandleFunc("POST /api/page/init_script", s.initScript)
	mux.HandleFunc("POST /api/page/touch", s.touch)
	mux.HandleFunc("POST /api/page/check", s.check)
	mux.HandleFunc("POST /api/page/profile", s.profile)
	mux.HandleFunc("POST /api/page/react", s.react)
	mux.HandleFunc("POST /api/page/extra_headers", s.extraHeaders)
	mux.HandleFunc("POST /api/page/user_agent", s.userAgent)
	mux.HandleFunc("POST /api/page/authenticate", s.authenticate)
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
	mux.HandleFunc("POST /api/page/key_down", s.keyDown)
	mux.HandleFunc("POST /api/page/key_up", s.keyUp)
	mux.HandleFunc("POST /api/page/focus", s.focusElement)
	mux.HandleFunc("GET /api/page/get", s.get)
	mux.HandleFunc("POST /api/page/get", s.get)
	mux.HandleFunc("POST /api/page/frame", s.frame)
	mux.HandleFunc("POST /api/page/clipboard", s.clipboard)
	mux.HandleFunc("POST /api/page/pushstate", s.pushState)
	mux.HandleFunc("POST /api/page/scroll", s.scroll)
	mux.HandleFunc("POST /api/page/wait_for", s.waitFor)
	mux.HandleFunc("POST /api/page/hover", s.hover)
	mux.HandleFunc("POST /api/page/evaluate", s.evaluate)
	mux.HandleFunc("GET /api/page/network_requests", s.networkRequests)
	mux.HandleFunc("POST /api/page/network_requests", s.networkRequests)
	mux.HandleFunc("GET /api/page/network_capture", s.networkCapture)
	mux.HandleFunc("POST /api/page/network_capture", s.networkCapture)
	mux.HandleFunc("POST /api/page/replay_request", s.replayRequest)
	mux.HandleFunc("POST /api/page/cookies", s.cookies)
	mux.HandleFunc("POST /api/browser/state", s.sessionState)
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
	mux.HandleFunc("POST /api/page/assert", s.assertPage)
	mux.HandleFunc("POST /api/page/click_xy", s.clickXY)
	mux.HandleFunc("GET /api/page/window_bounds", s.windowBounds)
	mux.HandleFunc("POST /api/browser/resize_window", s.resizeWindow)
	mux.HandleFunc("POST /api/page/vitals", s.vitals)
	mux.HandleFunc("POST /api/page/a11y", s.accessibilityAudit)
	mux.HandleFunc("POST /api/page/highlight", s.highlight)
	mux.HandleFunc("GET /api/page/console", s.consoleMessages)
	mux.HandleFunc("GET /api/page/downloads", s.downloads)
	mux.HandleFunc("GET /api/page/trace", s.trace)
	mux.HandleFunc("POST /api/page/clear_trace", s.clearTrace)
	mux.HandleFunc("POST /api/browser/group_tabs", s.groupTabs)
	mux.HandleFunc("POST /api/browser/ungroup_tabs", s.ungroupTabs)
	mux.HandleFunc("POST /api/visual/screenshot_save", s.screenshotSave)
	mux.HandleFunc("GET /api/visual/screenshot", s.screenshot)
	mux.HandleFunc("GET /api/visual/screenshot_element", s.screenshotElement)
	mux.HandleFunc("POST /api/artifacts/capture", s.captureArtifact)

	mux.HandleFunc("POST /api/artifacts/info", s.artifactInfo)
	mux.HandleFunc("POST /api/artifacts/read", s.readArtifact)
	mux.HandleFunc("POST /api/artifacts/search", s.searchArtifact)
	mux.HandleFunc("POST /api/artifacts/delete", s.deleteArtifact)
	mux.HandleFunc("GET /api/artifacts/{id}/info", s.artifactInfoLegacy)
	mux.HandleFunc("GET /api/artifacts/{id}/read", s.readArtifactLegacy)
	mux.HandleFunc("GET /api/artifacts/{id}/search", s.searchArtifactLegacy)
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.deleteArtifactLegacy)
	mux.HandleFunc("GET /api/plugins", s.listPlugins)
	mux.HandleFunc("POST /api/plugins/revoke", s.revokePlugin)
	mux.HandleFunc("POST /api/recipes/search", s.searchRecipes)
	mux.HandleFunc("POST /api/recipes/run", s.runRecipe)
	mux.HandleFunc("POST /api/baselines/route", s.routeBaseline)
	mux.HandleFunc("GET /api/skill", s.agentSkill)
	mux.HandleFunc("GET /api/consent/grants", s.consentGrants)
	mux.HandleFunc("POST /api/consent/revoke", s.consentRevoke)
}

type upstreamConsentSource interface {
	// UpstreamConsentPosture reports the posture of the daemon this controller forwards to, already merged with anything further upstream.
	UpstreamConsentPosture(ctx context.Context) (siteconsent.Posture, error)
}

const upstreamConsentTimeout = 3 * time.Second

func (s *Server) consentPosture(ctx context.Context) siteconsent.Posture {
	posture := s.consent.Posture()
	if s.upstreamConsent == nil {
		return posture
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamConsentTimeout)
	defer cancel()
	upstream, err := s.upstreamConsent.UpstreamConsentPosture(ctx)
	if err != nil {
		return posture.Merge(siteconsent.UnreadablePosture(fmt.Sprintf("the daemon this one forwards to could not be asked: %v", err)))
	}
	return posture.Merge(upstream)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"ok":         true,
		"tab_leases": s.leases.stats(),

		"consent": s.consentPosture(r.Context()),
	}
	if !s.identity.Empty() {
		payload["identity"] = s.identity
	}
	if s.version != "" {
		payload["version"] = s.version
	}

	if reporter, ok := s.manager.(browser.RemoteSessionReporter); ok {
		if session, live := reporter.RemoteSession(); live {
			payload["remote_session"] = session
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) captureArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeError(w, errors.New("artifact service is not configured on the browser host"))
		return
	}
	var req struct {
		artifact.CaptureOptions
		TabID string `json:"tab_id"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	result, err := s.artifacts.CaptureArtifact(s.contextWithTabID(r.Context(), req.TabID), req.CaptureOptions)
	writeResult(w, result, err)
}

func (s *Server) artifactInfo(w http.ResponseWriter, r *http.Request) {
	var req artifactIDRequest
	if !decodeArtifactRequest(w, r, &req) {
		return
	}
	s.artifactInfoForID(w, r, req.ArtifactID)
}

func (s *Server) artifactInfoLegacy(w http.ResponseWriter, r *http.Request) {
	markArtifactRouteDeprecated(w, "/api/artifacts/info")
	s.artifactInfoForID(w, r, r.PathValue("id"))
}

func (s *Server) artifactInfoForID(w http.ResponseWriter, r *http.Request, id string) {
	if s.artifacts == nil {
		writeError(w, errors.New("artifact service is not configured on the browser host"))
		return
	}
	result, err := s.artifacts.ArtifactInfo(r.Context(), id)
	writeArtifactResult(w, result, err)
}

func (s *Server) readArtifact(w http.ResponseWriter, r *http.Request) {
	var req artifactReadRequest
	if !decodeArtifactRequest(w, r, &req) {
		return
	}
	if req.Offset < 0 || req.MaxBytes < 0 || req.MaxBytes > artifact.MaxReadBytes {
		writeArtifactRequestError(w)
		return
	}
	s.readArtifactWindow(w, r, req.ArtifactID, req.Offset, req.MaxBytes)
}

func (s *Server) readArtifactLegacy(w http.ResponseWriter, r *http.Request) {
	markArtifactRouteDeprecated(w, "/api/artifacts/read")
	offset, ok := parseInt64Param(w, r.URL.Query().Get("offset"), "offset")
	if !ok {
		return
	}
	maxBytes, ok := parseIntParam(w, r.URL.Query().Get("max_bytes"), "max_bytes")
	if !ok {
		return
	}
	s.readArtifactWindow(w, r, r.PathValue("id"), offset, maxBytes)
}

func (s *Server) readArtifactWindow(w http.ResponseWriter, r *http.Request, id string, offset int64, maxBytes int) {
	if s.artifacts == nil {
		writeError(w, errors.New("artifact service is not configured on the browser host"))
		return
	}
	result, err := s.artifacts.ReadArtifact(r.Context(), id, offset, maxBytes)
	writeArtifactResult(w, result, err)
}

func (s *Server) searchArtifact(w http.ResponseWriter, r *http.Request) {
	var req artifactSearchRequest
	if !decodeArtifactRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" || len(req.Query) > 256 || !utf8.ValidString(req.Query) || req.Limit < 0 || req.Limit > 100 {
		writeArtifactRequestError(w)
		return
	}
	s.searchArtifactFor(w, r, req.ArtifactID, req.Query, req.Limit)
}

func (s *Server) searchArtifactLegacy(w http.ResponseWriter, r *http.Request) {
	markArtifactRouteDeprecated(w, "/api/artifacts/search")
	limit, ok := parseIntParam(w, r.URL.Query().Get("limit"), "limit")
	if !ok {
		return
	}
	s.searchArtifactFor(w, r, r.PathValue("id"), r.URL.Query().Get("query"), limit)
}

func (s *Server) searchArtifactFor(w http.ResponseWriter, r *http.Request, id, query string, limit int) {
	if s.artifacts == nil {
		writeError(w, errors.New("artifact service is not configured on the browser host"))
		return
	}
	result, err := s.artifacts.SearchArtifact(r.Context(), id, query, limit)
	writeArtifactResult(w, result, err)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	var req artifactIDRequest
	if !decodeArtifactRequest(w, r, &req) {
		return
	}
	s.deleteArtifactByID(w, r, req.ArtifactID)
}

func (s *Server) deleteArtifactLegacy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/artifacts/info" || r.URL.Path == "/api/artifacts/read" || r.URL.Path == "/api/artifacts/search" || r.URL.Path == "/api/artifacts/delete" {
		artifactPostOnly(w)
		return
	}
	markArtifactRouteDeprecated(w, "/api/artifacts/delete")
	s.deleteArtifactByID(w, r, r.PathValue("id"))
}

func (s *Server) deleteArtifactByID(w http.ResponseWriter, r *http.Request, id string) {
	if s.artifacts == nil {
		writeError(w, errors.New("artifact service is not configured on the browser host"))
		return
	}
	err := s.artifacts.DeleteArtifact(r.Context(), id)
	writeArtifactResult(w, map[string]any{"ok": err == nil}, err)
}

func artifactPostOnly(w http.ResponseWriter) {
	w.Header().Set("Allow", http.MethodPost)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

func markArtifactRouteDeprecated(w http.ResponseWriter, successor string) {
	w.Header().Set("Deprecation", "@0")
	w.Header().Set("Link", "<"+successor+">; rel=\"successor-version\"")
}

func (s *Server) searchRecipes(w http.ResponseWriter, r *http.Request) {
	if s.recipes == nil {
		writeError(w, errors.New("recipe provider is not configured on the browser host"))
		return
	}
	var req struct {
		Query  string `json:"query"`
		Origin string `json:"origin"`
		Limit  int    `json:"limit"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	result, err := s.recipes.SearchRecipes(r.Context(), req.Query, req.Origin, req.Limit)
	writeResult(w, result, err)
}

func (s *Server) routeBaseline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecipeDigest string `json:"recipe_digest"`
		PageURL      string `json:"page_url"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if s.baselineRoutes == nil {
		writeResult(w, baselineRouteResponse{Destination: recipe.BaselineLocal}, nil)
		return
	}
	route, err := s.baselineRoutes.RouteBaseline(r.Context(), req.RecipeDigest, req.PageURL)
	writeResult(w, baselineRouteResponse{Destination: route.Destination()}, err)
}

type baselineRouteResponse struct {
	Destination recipe.BaselineDestination `json:"destination"`
}

func (s *Server) runRecipe(w http.ResponseWriter, r *http.Request) {
	if s.recipes == nil {
		writeError(w, errors.New("recipe runtime is not configured on the browser host"))
		return
	}
	var req struct {
		recipe.RunRequest
		TabID string `json:"tab_id"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	result, err := s.recipes.RunRecipe(s.contextWithTabID(r.Context(), req.TabID), req.RunRequest)
	writeRunResult(w, result, err)
}

func writeRunResult(w http.ResponseWriter, result recipe.RunResult, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	w.Header().Set(usagelog.HeaderErrorClass, usagelog.ClassifyError(err))
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(err.Error()))
	writeJSON(w, http.StatusBadRequest, struct {
		recipe.RunResult
		Error string `json:"error"`
	}{RunResult: result, Error: err.Error()})
}

func (s *Server) requestContext(r *http.Request) context.Context {
	return s.contextWithTabID(r.Context(), r.URL.Query().Get("tab_id"))
}

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
			return browser.WithCurrentOwnedTabID(ctx, resolved)
		}
	}
	return ctx
}

func contextWithExplicitTabID(ctx context.Context, tabID string) context.Context {
	if tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

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

		daemonGrouped = true
		result, err = s.openInOwnerGroup(r.Context(), req.URL, owner)
	default:
		result, err = s.manager.Open(r.Context(), req.URL)
	}
	if s.discardIfAbandoned(r.Context(), result) {
		writeResult(w, browser.OpenResult{}, r.Context().Err())
		return
	}
	if err == nil {
		err = s.leases.bind(owner, result.Tab.ID, true)
	}
	if err == nil {
		s.leases.noteOpened(owner, result.Tab.ID)
	}
	if err == nil && daemonGrouped {
		s.leases.noteGroup(owner, result.Tab.ID, result.Tab.GroupID)
	}
	writeResult(w, result, err)
}

func (s *Server) discardIfAbandoned(ctx context.Context, result browser.OpenResult) bool {
	if ctx.Err() == nil || strings.TrimSpace(result.Tab.ID) == "" {
		return false
	}
	s.closeAbandonedTab(ctx, result.Tab.ID)
	return true
}

const abandonedTabCloseTimeout = 10 * time.Second

func (s *Server) closeAbandonedTab(ctx context.Context, tabID string) {
	if strings.TrimSpace(tabID) == "" {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonedTabCloseTimeout)
	defer cancel()

	_ = s.manager.CloseTab(closeCtx, tabID)
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
	if s.discardIfAbandoned(r.Context(), result) {
		writeResult(w, browser.OpenResult{}, r.Context().Err())
		return
	}
	owner := leaseOwner(r.Context())
	if err == nil {
		err = s.leases.bind(owner, result.Tab.ID, true)
	}
	if err == nil {
		s.leases.noteOpened(owner, result.Tab.ID)
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
		if owned, ok := s.pageWatch.(interface{ OwnsTab(string) bool }); ok {
			for i := range tabs {
				if owned.OwnsTab(tabs[i].ID) {
					tabs[i].Lease = &browser.TabLeaseInfo{Status: "leased"}
				}
			}
		}
	}
	writeResult(w, tabs, err)
}

func (s *Server) activeTab(w http.ResponseWriter, r *http.Request) {

	if owner := leaseOwner(r.Context()); owner != "" {
		if tabID, release, ok := s.leases.acquireDefault(owner); ok {
			release()
			writeJSON(w, http.StatusOK, map[string]string{"tab_id": tabID})
			return
		}
	}
	reporter, ok := s.manager.(browser.ActiveTabReporter)
	if !ok {
		writeError(w, errors.New("this browser transport cannot name its active tab"))
		return
	}
	tabID, err := reporter.ActiveTabID(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"tab_id": tabID})
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
	release, err := s.leases.acquireFor(r.Context(), owner, tabID, true)
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
	release, err := s.leases.acquireFor(r.Context(), owner, tabID, false)
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

func (s *Server) releaseSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CloseTabs bool `json:"close_tabs"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	owner := leaseOwner(r.Context())
	if owner == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "no session owner: send " + usagelog.HeaderOwnerID + " or " + usagelog.HeaderSessionID,
		})
		return
	}
	closed := []string{}
	closeErrors := map[string]string{}
	if req.CloseTabs {
		_, opened := s.leases.ownedTabs(owner)
		for _, tabID := range opened {
			err := s.manager.CloseTab(r.Context(), tabID)
			if err == nil || usagelog.ClassifyError(err) == "tab_lost" {
				closed = append(closed, tabID)
				continue
			}
			closeErrors[tabID] = err.Error()
		}
	}
	released := s.leases.releaseOwner(owner)
	if released == nil {
		released = []string{}
	}
	out := map[string]any{"ok": len(closeErrors) == 0, "released": released, "closed": closed}
	if len(closeErrors) > 0 {
		out["close_errors"] = closeErrors
	}
	writeJSON(w, http.StatusOK, out)
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
	opts, live, ok := parseFindOptions(w, r)
	if !ok {
		return
	}
	ctx := s.requestContext(r)
	if !live {
		result, err := s.manager.Find(ctx, opts)
		writeResult(w, result, err)
		return
	}
	result, err := s.manager.FindLive(ctx, opts)
	if err == nil {
		if result.Metadata == nil {
			result.Metadata = map[string]any{}
		}
		result.Metadata[snapshot.FindLiveKey] = true
	}
	writeResult(w, result, err)
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	opts, ok := parseReadOptions(w, r)
	if !ok {
		return
	}
	ctx := s.requestContext(r)
	if opts.SettleMS != nil {
		ctx = readability.WithSettleMS(ctx, *opts.SettleMS)
	}
	read, err := s.manager.Read(ctx)
	if err != nil {
		writeResult(w, read, err)
		return
	}

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

func parseReadOptions(w http.ResponseWriter, r *http.Request) (readability.ReadOptions, bool) {
	q := r.URL.Query()
	opts := readability.ReadOptions{}
	if q.Has("settle_ms") {
		ms, err := strconv.Atoi(q.Get("settle_ms"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "settle_ms must be an integer from 0 to 5000"})
			return opts, false
		}
		opts.SettleMS = &ms
	}

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
	if _, ok := err.(*tabLeaseConflictError); ok {
		writeLeaseError(w, err)
		return
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

func (s *Server) keyDown(w http.ResponseWriter, r *http.Request) {
	s.keyHalf(w, r, true)
}

func (s *Server) keyUp(w http.ResponseWriter, r *http.Request) {
	s.keyHalf(w, r, false)
}

func (s *Server) keyHalf(w http.ResponseWriter, r *http.Request, down bool) {
	keys, ok := s.manager.(browser.KeyHoldController)
	if !ok {
		writeError(w, errors.New("this browser transport does not support held keys: use /api/page/press for a discrete keystroke"))
		return
	}
	var req browser.KeyHoldOptions
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if down {
		result, err := keys.KeyDown(ctx, req)
		writeResult(w, result, err)
		return
	}
	result, err := keys.KeyUp(ctx, req)
	writeResult(w, result, err)
}

func (s *Server) focusElement(w http.ResponseWriter, r *http.Request) {
	focuser, ok := s.manager.(browser.ElementFocuser)
	if !ok {
		writeError(w, errors.New("this browser transport does not support explicit element focus"))
		return
	}
	var req struct {
		Ref      string `json:"ref"`
		Snapshot bool   `json:"snapshot"`
		TabID    string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Ref) == "" {
		writeError(w, errors.New("ref is required"))
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if req.Snapshot {
		ctx = browser.WithWantSnapshot(ctx)
	}
	result, err := focuser.Focus(ctx, req.Ref)
	writeResult(w, result, err)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	var req snapshot.GetRequest
	if r.Method == http.MethodGet {
		query := r.URL.Query()
		req = snapshot.GetRequest{
			What:   query.Get("what"),
			Target: query.Get("target"),
			Name:   query.Get("name"),
			TabID:  query.Get("tab_id"),
		}
	} else if !decode(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err)
		return
	}
	ctx := browser.WithTraceLabel(s.contextWithTabID(r.Context(), req.TabID), browser.TraceActionGet, req.TraceLabel())
	value, err := s.manager.Evaluate(ctx, req.Expression())
	writeResult(w, value, err)
}

func (s *Server) frame(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
		TabID  string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := browser.WithTraceLabel(s.contextWithTabID(r.Context(), req.TabID), browser.TraceActionFrame, req.Target)
	value, err := s.manager.Evaluate(ctx, snapshot.BuildFrameSwitchExpression(req.Target))
	writeResult(w, value, err)
}

func (s *Server) clipboard(w http.ResponseWriter, r *http.Request) {
	clipboard, ok := s.manager.(browser.ClipboardController)
	if !ok {
		writeError(w, errors.New("this browser transport does not support clipboard access: granting the clipboard permission needs a browser-level CDP command the extension bridge cannot send"))
		return
	}
	var req browser.ClipboardOptions
	if !decode(w, r, &req) {
		return
	}
	result, err := clipboard.Clipboard(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) pushState(w http.ResponseWriter, r *http.Request) {
	history, ok := s.manager.(browser.HistoryController)
	if !ok {
		writeError(w, errors.New("this browser transport does not support same-document history changes"))
		return
	}
	var req browser.HistoryStateOptions
	if !decode(w, r, &req) {
		return
	}

	if s.denyNav(w, req.URL) {
		return
	}
	result, err := history.PushState(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
		Target    string `json:"target"`
		Ref       string `json:"ref"`
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
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = strings.TrimSpace(req.Ref)
	}
	if target != "" {
		st, ok := s.manager.(browser.ScrollToController)
		if !ok {
			writeError(w, errors.New("scrolling an element into view is not available on this transport"))
			return
		}
		result, err := st.ScrollTo(ctx, target)
		writeResult(w, result, err)
		return
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
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond

	if observer, ok := s.manager.(browser.WaitObserver); ok {
		outcome, err := observer.WaitForOutcome(ctx, req.Condition, timeout)
		writeResult(w, outcome, err)
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.WaitFor(ctx, req.Condition, timeout))
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
		// TraceAction/TraceValue carry the label a proxied brw_get or brw_frame applied on its own side.
		TraceAction string `json:"trace_action"`
		TraceValue  string `json:"trace_value"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	if browser.IsGeneratedScriptVerb(req.TraceAction) {
		ctx = browser.WithTraceLabel(ctx, req.TraceAction, req.TraceValue)
	}
	result, err := s.manager.Evaluate(ctx, req.Expression)
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

func (s *Server) cookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		browser.CookieParams
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Cookies(s.contextWithTabID(r.Context(), req.TabID), req.CookieParams)
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

func (s *Server) assertPage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		browser.AssertRequest
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := browser.Assert(s.contextWithTabID(r.Context(), req.TabID), s.manager, req.AssertRequest)
	writeAssertResult(w, result, err)
}

func writeAssertResult(w http.ResponseWriter, result browser.AssertResult, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	w.Header().Set(usagelog.HeaderErrorClass, usagelog.ClassifyError(err))
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(err.Error()))
	writeJSON(w, http.StatusBadRequest, struct {
		browser.AssertResult
		Error string `json:"error"`
	}{AssertResult: result, Error: err.Error()})
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

const defaultHTTPConsoleLimit = 100

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

func (s *Server) scopedTrace(r *http.Request) browser.TraceResult {
	full := s.manager.GetTrace()
	owner := leaseOwner(r.Context())
	if owner == "" {

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

func parseFindOptions(w http.ResponseWriter, r *http.Request) (snapshot.FindOptions, bool, bool) {
	if r.Method == http.MethodPost {
		var req struct {
			snapshot.FindOptions
			Live bool `json:"live"`
		}
		if !decode(w, r, &req) {
			return snapshot.FindOptions{}, false, false
		}
		if req.Limit < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be non-negative"})
			return snapshot.FindOptions{}, false, false
		}
		return req.FindOptions, req.Live, true
	}
	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit")
	if !ok {
		return snapshot.FindOptions{}, false, false
	}
	viewportOnly, ok := parseBoolValue(w, q.Get("viewport_only"), "viewport_only")
	if !ok {
		return snapshot.FindOptions{}, false, false
	}
	includeHidden, ok := parseBoolValue(w, q.Get("include_hidden"), "include_hidden")
	if !ok {
		return snapshot.FindOptions{}, false, false
	}
	textContent, ok := parseBoolValue(w, q.Get("text_content"), "text_content")
	if !ok {
		return snapshot.FindOptions{}, false, false
	}
	live, ok := parseBoolValue(w, q.Get(snapshot.FindLiveKey), snapshot.FindLiveKey)
	if !ok {
		return snapshot.FindOptions{}, false, false
	}
	return snapshot.FindOptions{
		Query:         q.Get("query"),
		Role:          q.Get("role"),
		Text:          q.Get("text"),
		Limit:         limit,
		ViewportOnly:  viewportOnly,
		IncludeHidden: includeHidden,
		TextContent:   textContent,
	}, live, true
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
	data, err := json.Marshal(snap)
	if err != nil || len(data) <= maxBytes {
		return snap
	}
	elements := snap.Elements
	count := sort.Search(len(elements), func(i int) bool {
		snap.Elements = elements[:i+1]
		data, _ := json.Marshal(snap)
		return len(data) > maxBytes
	})
	snap.Elements = elements[:count]
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

const maxRequestBodyBytes = 8 << 20

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeBody(w, r, dst, maxRequestBodyBytes, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeBody(w, r, dst, maxRequestBodyBytes, true); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64, strict bool) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func decodeArtifactRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeBody(w, r, dst, maxArtifactRequestBodyBytes, true); err != nil {
		writeArtifactRequestError(w)
		return false
	}
	return true
}

func writeArtifactRequestError(w http.ResponseWriter) {
	w.Header().Set(usagelog.HeaderErrorClass, "invalid_argument")
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint("invalid artifact request"))
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid artifact request"})
}

func writeArtifactResult(w http.ResponseWriter, value any, err error) {
	if err != nil {

		const publicMessage = "artifact operation failed"
		errorClass, fingerprintMessage := artifactFailureUsage(err)
		w.Header().Set(usagelog.HeaderErrorClass, errorClass)
		w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(fingerprintMessage))
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": publicMessage})
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func artifactFailureUsage(err error) (errorClass, fingerprintMessage string) {
	errorClass = usagelog.ClassifyError(err)
	switch errorClass {
	case "not_found":
		return "artifact_not_found", "artifact not found"
	case "invalid_argument":
		return errorClass, "invalid artifact request"
	case "tool", "":
		return "artifact_error", "artifact operation failed"
	default:
		return errorClass, "artifact operation failed"
	}
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeError(w http.ResponseWriter, err error) {
	if detail := approvalgate.ErrorDetails(err); detail != nil {
		w.Header().Set(usagelog.HeaderErrorClass, "approval_required")
		w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint("approval required"))
		writeJSON(w, http.StatusConflict, detail)
		return
	}
	var refused *browser.TakeoverRefusedError
	if errors.As(err, &refused) {
		w.Header().Set(usagelog.HeaderErrorClass, usagelog.ClassifyError(err))

		w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint("takeover held"))

		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      err.Error(),
			"code":       browser.TakeoverRefusedCode,
			"action":     refused.Action,
			"holder":     refused.Holder,
			"expires_at": refused.ExpiresAt,
		})
		return
	}
	w.Header().Set(usagelog.HeaderErrorClass, usagelog.ClassifyError(err))
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(err.Error()))
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
