package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type Controller interface {
	Open(context.Context, string) (browser.OpenResult, error)
	ListTabs(context.Context) ([]browser.Tab, error)
	FocusTab(context.Context, string) error
	CloseTab(context.Context, string) error
	Read(context.Context) (readability.PageRead, error)
	Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Click(context.Context, string) (browser.ActionResult, error)
	Type(context.Context, string, string) (browser.ActionResult, error)
	Fill(context.Context, snapshot.FillOptions) (browser.ActionResult, error)
	UploadFile(context.Context, snapshot.UploadOptions) (browser.ActionResult, error)
	Select(context.Context, string, string) (browser.ActionResult, error)
	Press(context.Context, string) (browser.ActionResult, error)
	Scroll(context.Context, string) (browser.ActionResult, error)
	WaitFor(context.Context, string, time.Duration) error
	Screenshot(context.Context) (browser.Screenshot, error)
	ScreenshotElement(context.Context, string) (browser.Screenshot, error)
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
	mux.HandleFunc("GET /api/browser/tabs", s.tabs)
	mux.HandleFunc("POST /api/browser/focus", s.focus)
	mux.HandleFunc("POST /api/browser/close", s.closeTab)
	mux.HandleFunc("GET /api/page/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/page/find", s.find)
	mux.HandleFunc("POST /api/page/find", s.find)
	mux.HandleFunc("GET /api/page/read", s.read)
	mux.HandleFunc("POST /api/page/click", s.click)
	mux.HandleFunc("POST /api/page/type", s.typeText)
	mux.HandleFunc("POST /api/page/fill", s.fill)
	mux.HandleFunc("POST /api/page/upload_file", s.uploadFile)
	mux.HandleFunc("POST /api/page/select", s.selectValue)
	mux.HandleFunc("POST /api/page/press", s.press)
	mux.HandleFunc("POST /api/page/scroll", s.scroll)
	mux.HandleFunc("POST /api/page/wait_for", s.waitFor)
	mux.HandleFunc("GET /api/visual/screenshot", s.screenshot)
	mux.HandleFunc("GET /api/visual/screenshot_element", s.screenshotElement)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	snap, err := s.manager.Snapshot(r.Context(), req.Options)
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
	result, err := s.manager.Find(r.Context(), opts)
	writeResult(w, result, err)
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	read, err := s.manager.Read(r.Context())
	writeResult(w, read, err)
}

func (s *Server) click(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Click(r.Context(), req.Ref)
	writeResult(w, result, err)
}

func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref  string `json:"ref"`
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Type(r.Context(), req.Ref, req.Text)
	writeResult(w, result, err)
}

func (s *Server) fill(w http.ResponseWriter, r *http.Request) {
	req := snapshot.FillOptions{Replace: true}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Fill(r.Context(), req)
	writeResult(w, result, err)
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	var req snapshot.UploadOptions
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.UploadFile(r.Context(), req)
	writeResult(w, result, err)
}

func (s *Server) selectValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		Value string `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Select(r.Context(), req.Ref, req.Value)
	writeResult(w, result, err)
}

func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Press(r.Context(), req.Key)
	writeResult(w, result, err)
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.manager.Scroll(r.Context(), req.Direction)
	writeResult(w, result, err)
}

func (s *Server) waitFor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Condition string `json:"condition"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.WaitFor(r.Context(), req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	shot, err := s.manager.Screenshot(r.Context())
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
	shot, err := s.manager.ScreenshotElement(r.Context(), ref)
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
