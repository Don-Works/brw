package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type Controller interface {
	Open(context.Context, string) (browser.OpenResult, error)
	OpenInGroup(context.Context, string, string) (browser.OpenResult, error)
	OpenIncognito(context.Context, string) (browser.OpenResult, error)
	CloseContext(context.Context, string) error
	ListTabs(context.Context) ([]browser.Tab, error)
	FocusTab(context.Context, string) error
	CloseTab(context.Context, string) error
	GroupTabs(context.Context, []string, string, string) error
	UngroupTabs(context.Context, []string) error
	Read(context.Context) (readability.PageRead, error)
	ReadData(context.Context) (snapshot.StructuredData, error)
	Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Click(context.Context, string) (browser.ActionResult, error)
	ClickText(context.Context, snapshot.ClickTextOptions) (browser.ActionResult, error)
	Navigate(context.Context, string) (browser.ActionResult, error)
	ClickButton(context.Context, browser.ClickButtonOptions) (browser.ActionResult, error)
	MouseDown(context.Context, browser.MouseButtonOptions) (browser.ActionResult, error)
	MouseUp(context.Context, browser.MouseButtonOptions) (browser.ActionResult, error)
	Drag(context.Context, browser.DragOptions) (browser.ActionResult, error)
	Hover(context.Context, string) (browser.ActionResult, error)
	Type(context.Context, string, string) (browser.ActionResult, error)
	Fill(context.Context, snapshot.FillOptions) (browser.ActionResult, error)
	UploadFile(context.Context, snapshot.UploadOptions) (browser.ActionResult, error)
	Select(context.Context, string, string) (browser.ActionResult, error)
	Press(context.Context, string) (browser.ActionResult, error)
	Scroll(context.Context, string) (browser.ActionResult, error)
	WaitFor(context.Context, string, time.Duration) error
	Screenshot(context.Context) (browser.Screenshot, error)
	ScreenshotElement(context.Context, string) (browser.Screenshot, error)
	Evaluate(context.Context, string) (any, error)
	NetworkRequests(context.Context, string) ([]browser.NetworkRequest, error)
	NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error)
	ReplayRequest(context.Context, browser.ReplayRequestParams) (snapshot.ReplayResult, error)
	ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error)
	ExecuteBatch(context.Context, []browser.BatchStep) (browser.BatchResult, error)
	Cancel(context.Context, string) (browser.CancelResult, error)
	Observe(context.Context) (browser.ObserveResult, error)
	ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error)
	Downloads(context.Context) (browser.DownloadsResult, error)
	ClickXY(context.Context, float64, float64) (snapshot.ClickXYResult, error)
	GetTrace() browser.TraceResult
	ClearTrace()
	AssertVisible(context.Context, string, time.Duration) error
	AssertText(context.Context, string, string, time.Duration) error
	AssertValue(context.Context, string, string, time.Duration) error
	AssertHidden(context.Context, string, time.Duration) error
	CommitField(context.Context, string) error
	Notify(context.Context, browser.NotifyOptions) (browser.NotifyResult, error)
	GetPolicy(context.Context) (browser.PolicySettings, error)
	SetPolicy(context.Context, browser.PolicySettings) (browser.PolicySettings, error)
}

type Server struct {
	manager Controller
	server  *http.Server
}

type snapshotRequest struct {
	Options  snapshot.SnapshotOptions
	MaxBytes int
}

func New(addr string, manager Controller) *Server {
	mux := http.NewServeMux()
	s := &Server{manager: manager, server: &http.Server{Addr: addr, Handler: mux}}
	s.routes(mux)
	return s
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
	mux.HandleFunc("POST /api/browser/focus", s.focus)
	mux.HandleFunc("POST /api/browser/close", s.closeTab)
	mux.HandleFunc("GET /api/page/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/page/find", s.find)
	mux.HandleFunc("POST /api/page/find", s.find)
	mux.HandleFunc("GET /api/page/read", s.read)
	mux.HandleFunc("GET /api/page/read_data", s.readData)
	mux.HandleFunc("POST /api/page/click", s.click)
	mux.HandleFunc("POST /api/page/click_text", s.clickText)
	mux.HandleFunc("POST /api/page/navigate", s.navigate)
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
	mux.HandleFunc("GET /api/page/console", s.consoleMessages)
	mux.HandleFunc("GET /api/page/downloads", s.downloads)
	mux.HandleFunc("GET /api/page/trace", s.trace)
	mux.HandleFunc("POST /api/page/clear_trace", s.clearTrace)
	mux.HandleFunc("GET /api/browser/policy", s.getPolicy)
	mux.HandleFunc("POST /api/browser/policy", s.setPolicy)
	mux.HandleFunc("POST /api/browser/group_tabs", s.groupTabs)
	mux.HandleFunc("POST /api/browser/ungroup_tabs", s.ungroupTabs)
	mux.HandleFunc("GET /api/visual/screenshot", s.screenshot)
	mux.HandleFunc("GET /api/visual/screenshot_element", s.screenshotElement)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func requestContext(r *http.Request) context.Context {
	return contextWithTabID(r.Context(), r.URL.Query().Get("tab_id"))
}

func contextWithTabID(ctx context.Context, tabID string) context.Context {
	if tabID == "" {
		return ctx
	}
	return browser.WithTabID(ctx, tabID)
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Open(r.Context(), req.URL)
	writeResult(w, result, err)
}

func (s *Server) openIncognito(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.OpenIncognito(r.Context(), req.URL)
	writeResult(w, result, err)
}

func (s *Server) closeContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BrowserContextID string `json:"browser_context_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CloseContext(r.Context(), req.BrowserContextID))
}

func (s *Server) tabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := s.manager.ListTabs(r.Context())
	writeResult(w, tabs, err)
}

func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.FocusTab(r.Context(), req.ID))
}

func (s *Server) closeTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CloseTab(r.Context(), req.ID))
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	req, ok := parseSnapshotOptions(w, r)
	if !ok {
		return
	}
	snap, err := s.manager.Snapshot(requestContext(r), req.Options)
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
	result, err := s.manager.Find(requestContext(r), opts)
	writeResult(w, result, err)
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	read, err := s.manager.Read(requestContext(r))
	writeResult(w, read, err)
}

func (s *Server) readData(w http.ResponseWriter, r *http.Request) {
	data, err := s.manager.ReadData(requestContext(r))
	writeResult(w, data, err)
}

func (s *Server) click(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref        string   `json:"ref"`
		X          *float64 `json:"x"`
		Y          *float64 `json:"y"`
		Button     string   `json:"button"`
		ClickCount int      `json:"click_count"`
		TabID      string   `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := contextWithTabID(r.Context(), req.TabID)
	if isDefaultLeftSingleRefClick(req.Button, req.ClickCount, req.Ref, req.X, req.Y) {
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

// isDefaultLeftSingleRefClick reports whether a click is a plain left
// single-click on a ref, which keeps the optimized in-page click path.
func isDefaultLeftSingleRefClick(button string, clickCount int, ref string, x, y *float64) bool {
	if x != nil || y != nil || ref == "" || clickCount > 1 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		return true
	default:
		return false
	}
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
	result, err := s.manager.Drag(contextWithTabID(r.Context(), req.TabID), browser.DragOptions{
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
	result, err := s.manager.MouseDown(contextWithTabID(r.Context(), tabID), opts)
	writeResult(w, result, err)
}

func (s *Server) mouseUp(w http.ResponseWriter, r *http.Request) {
	opts, tabID, ok := decodeMouseButton(w, r)
	if !ok {
		return
	}
	result, err := s.manager.MouseUp(contextWithTabID(r.Context(), tabID), opts)
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
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.ClickText(contextWithTabID(r.Context(), req.TabID), req.ClickTextOptions)
	writeResult(w, result, err)
}

func (s *Server) navigate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Navigate(contextWithTabID(r.Context(), req.TabID), req.Direction)
	writeResult(w, result, err)
}

func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		Text  string `json:"text"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Type(contextWithTabID(r.Context(), req.TabID), req.Ref, req.Text)
	writeResult(w, result, err)
}

func (s *Server) fill(w http.ResponseWriter, r *http.Request) {
	req := struct {
		snapshot.FillOptions
		TabID string `json:"tab_id"`
	}{FillOptions: snapshot.FillOptions{Replace: true}}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Fill(contextWithTabID(r.Context(), req.TabID), req.FillOptions)
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
	result, err := s.manager.UploadFile(contextWithTabID(r.Context(), req.TabID), req.UploadOptions)
	writeResult(w, result, err)
}

func (s *Server) selectValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		Value string `json:"value"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Select(contextWithTabID(r.Context(), req.TabID), req.Ref, req.Value)
	writeResult(w, result, err)
}

func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Press(contextWithTabID(r.Context(), req.TabID), req.Key)
	writeResult(w, result, err)
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
		TabID     string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Scroll(contextWithTabID(r.Context(), req.TabID), req.Direction)
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.WaitFor(contextWithTabID(r.Context(), req.TabID), req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) hover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		TabID string `json:"tab_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Hover(contextWithTabID(r.Context(), req.TabID), req.Ref)
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
	result, err := s.manager.Evaluate(contextWithTabID(r.Context(), req.TabID), req.Expression)
	writeResult(w, result, err)
}

func (s *Server) networkRequests(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	ctx := requestContext(r)
	if r.Method == http.MethodPost {
		var req struct {
			Filter string `json:"filter"`
			TabID  string `json:"tab_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		filter = req.Filter
		ctx = contextWithTabID(r.Context(), req.TabID)
	}
	result, err := s.manager.NetworkRequests(ctx, filter)
	writeResult(w, result, err)
}

func (s *Server) networkCapture(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	ctx := requestContext(r)
	if r.Method == http.MethodPost {
		var req struct {
			Filter string `json:"filter"`
			TabID  string `json:"tab_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		filter = req.Filter
		ctx = contextWithTabID(r.Context(), req.TabID)
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
	result, err := s.manager.ReplayRequest(contextWithTabID(r.Context(), req.TabID), browser.ReplayRequestParams{
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
	result, err := s.manager.ExecutePlan(contextWithTabID(r.Context(), req.TabID), req.Steps)
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
	result, err := s.manager.ExecuteBatch(contextWithTabID(r.Context(), req.TabID), req.Steps)
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
	result, err := s.manager.Cancel(contextWithTabID(r.Context(), req.TabID), req.Token)
	writeResult(w, result, err)
}

func (s *Server) observe(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.Observe(requestContext(r))
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.CommitField(contextWithTabID(r.Context(), req.TabID), req.Ref))
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
	result, err := s.manager.Notify(contextWithTabID(r.Context(), req.TabID), browser.NotifyOptions{Kind: req.Kind, Title: req.Title, Message: req.Message})
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertVisible(contextWithTabID(r.Context(), req.TabID), req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertHidden(contextWithTabID(r.Context(), req.TabID), req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertText(contextWithTabID(r.Context(), req.TabID), req.Ref, req.Text, time.Duration(req.TimeoutMS)*time.Millisecond))
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.AssertValue(contextWithTabID(r.Context(), req.TabID), req.Ref, req.Value, time.Duration(req.TimeoutMS)*time.Millisecond))
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
	result, err := s.manager.ClickXY(contextWithTabID(r.Context(), req.TabID), req.X, req.Y)
	writeResult(w, result, err)
}

func (s *Server) consoleMessages(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.ConsoleMessages(requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	result, err := s.manager.Downloads(requestContext(r))
	writeResult(w, result, err)
}

func (s *Server) trace(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.GetTrace())
}

func (s *Server) clearTrace(w http.ResponseWriter, _ *http.Request) {
	s.manager.ClearTrace()
	writeJSON(w, http.StatusOK, browser.ActionResult{OK: true})
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	settings, err := s.manager.GetPolicy(r.Context())
	writeResult(w, settings, err)
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) {
	var req browser.PolicySettings
	if !decode(w, r, &req) {
		return
	}
	settings, err := s.manager.SetPolicy(r.Context(), req)
	writeResult(w, settings, err)
}

func (s *Server) groupTabs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabIDs []string `json:"tab_ids"`
		Name   string   `json:"name"`
		Color  string   `json:"color"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.GroupTabs(r.Context(), req.TabIDs, req.Name, req.Color))
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
	shot, err := s.manager.Screenshot(requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("base64") == "1" {
		writeJSON(w, http.StatusOK, shot)
		return
	}
	w.Header().Set("content-type", shot.MIMEType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(shot.Data)
}

func (s *Server) screenshotElement(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	shot, err := s.manager.ScreenshotElement(requestContext(r), ref)
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
		Options: snapshot.SnapshotOptions{
			Mode:          q.Get("mode"),
			Query:         q.Get("query"),
			Role:          q.Get("role"),
			Text:          q.Get("text"),
			Limit:         limit,
			ViewportOnly:  viewportOnly,
			IncludeHidden: includeHidden,
			IncludeAX:     includeAX,
			Since:         since,
		},
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
	return snapshot.FindOptions{
		Query:         q.Get("query"),
		Role:          q.Get("role"),
		Text:          q.Get("text"),
		Limit:         limit,
		ViewportOnly:  viewportOnly,
		IncludeHidden: includeHidden,
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

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
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
