package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
)

type Server struct {
	manager *browser.Manager
	server  *http.Server
}

func New(addr string, manager *browser.Manager) *Server {
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
	mux.HandleFunc("GET /api/page/read", s.read)
	mux.HandleFunc("POST /api/page/click", s.click)
	mux.HandleFunc("POST /api/page/type", s.typeText)
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
	snap, err := s.manager.Snapshot(r.Context())
	writeResult(w, snap, err)
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
	writeResult(w, browser.ActionResult{OK: true}, s.manager.Click(r.Context(), req.Ref))
}

func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref  string `json:"ref"`
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.Type(r.Context(), req.Ref, req.Text))
}

func (s *Server) selectValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref   string `json:"ref"`
		Value string `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.Select(r.Context(), req.Ref, req.Value))
}

func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.Press(r.Context(), req.Key))
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Direction string `json:"direction"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeResult(w, browser.ActionResult{OK: true}, s.manager.Scroll(r.Context(), req.Direction))
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
