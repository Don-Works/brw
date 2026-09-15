package httpapi

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/profileroster"
	"github.com/Don-Works/brw/internal/setup"
)

var rosterPageHTML []byte

func rosterPolicyPath() string {
	if p := strings.TrimSpace(os.Getenv("BRW_PROFILE_POLICY")); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	path, err := setup.DefaultPolicyPath(home)
	if err != nil || path == "" {
		path, _ = profilepolicy.Discover("")
	}
	return path
}

func (s *Server) rosterGuard(w http.ResponseWriter, r *http.Request) bool {
	if !requestIsLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "the profile manager only serves loopback clients",
		})
		return false
	}
	return true
}

func (s *Server) rosterPage(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(rosterPageHTML)
}

func (s *Server) rosterBoard(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	board, err := profileroster.LoadBoard(r.Context(), rosterPolicyPath())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) rosterSelf(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	inv, ok := s.manager.(interface {
		AllCookies(context.Context) ([]browser.Cookie, error)
	})
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"cookies": []browser.CookieMeta{}})
		return
	}
	all, err := inv.AllCookies(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"cookies": []browser.CookieMeta{}, "error": err.Error()})
		return
	}
	metas := make([]browser.CookieMeta, 0, len(all))
	for _, c := range all {
		metas = append(metas, c.Meta())
	}
	writeJSON(w, http.StatusOK, map[string]any{"cookies": metas})
}

func (s *Server) rosterCreate(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Account string `json:"account"`
		Browser string `json:"browser"`
	}
	if !decode(w, r, &req) {
		return
	}
	home, _ := os.UserHomeDir()
	brwd := strings.TrimSpace(os.Getenv("BRWD"))
	if brwd == "" {
		if exe, err := os.Executable(); err == nil {
			brwd = exe
		}
	}
	result, err := profileroster.Create(profileroster.CreateRequest{
		Name:           req.Name,
		Account:        req.Account,
		Browser:        req.Browser,
		PolicyPath:     rosterPolicyPath(),
		Home:           home,
		InstallService: true,
		BRWDPath:       brwd,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) rosterCopy(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	var req struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Domain string `json:"domain"`
		Mode   string `json:"mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	policy, err := profilepolicy.Load(rosterPolicyPath())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	result, err := profileroster.CopyDomain(r.Context(), policy, req.From, req.To, req.Domain, req.Mode)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) rosterPin(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	var req struct {
		Profile string `json:"profile"`
		Origin  string `json:"origin"`
		Account string `json:"account"`
		Label   string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := profileroster.AddPin(rosterPolicyPath(), req.Profile, req.Origin, req.Account, req.Label); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) rosterOpen(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	var req struct {
		Profile string `json:"profile"`
		URL     string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		req.URL = "https://accounts.google.com"
	}
	if s.identity.Profile == req.Profile || req.Profile == "" {
		result, err := s.manager.Open(s.contextWithTabID(r.Context(), ""), req.URL)
		writeResult(w, result, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "start that profile's daemon and open the site from its well",
		"url":     req.URL,
	})
}
