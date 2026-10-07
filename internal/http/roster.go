package httpapi

import (
	"context"
	_ "embed"
	"net/http"
	"os"
	"strings"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

//go:embed roster.html
var rosterHTML []byte

//go:embed roster.js
var rosterJS []byte

// ProfileRoster is what the /profiles page drives.
type ProfileRoster interface {
	Board(ctx context.Context, policyPath string) (any, error)
	Create(policyPath, name, account, browserName string) (any, error)
	Copy(ctx context.Context, policyPath, from, to, domain, mode string) (any, error)
	Pin(policyPath, profile, origin, account, label string) error
	Open(ctx context.Context, policyPath, profile, rawURL string) (any, error)
	Refused(error) bool
}

// SetProfileRoster installs the profile roster behind /profiles and /api/roster/*.
func (s *Server) SetProfileRoster(roster ProfileRoster) { s.roster = roster }

// SetProfilePolicyPath names the policy file the profile roster reads and edits.
func (s *Server) SetProfilePolicyPath(path string) { s.profilePolicyPath = strings.TrimSpace(path) }

func (s *Server) rosterPolicyPath() (string, error) {
	if s.profilePolicyPath != "" {
		return s.profilePolicyPath, nil
	}
	if path, err := profilepolicy.Discover(""); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return setup.DefaultPolicyPath(home)
}

func (s *Server) rosterGuard(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !requestIsLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "the profile roster only serves loopback clients",
		})
		return "", false
	}
	if s.roster == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "this daemon has no profile roster"})
		return "", false
	}
	path, err := s.rosterPolicyPath()
	if err != nil {
		s.rosterResult(w, nil, err)
		return "", false
	}
	return path, true
}

func (s *Server) rosterResult(w http.ResponseWriter, result any, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, result)
	case s.roster != nil && s.roster.Refused(err):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
}

func (s *Server) rosterPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.rosterGuard(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	_, _ = w.Write(rosterHTML)
}

func (s *Server) rosterScript(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.rosterGuard(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(rosterJS)
}

func (s *Server) rosterBoard(w http.ResponseWriter, r *http.Request) {
	path, ok := s.rosterGuard(w, r)
	if !ok {
		return
	}
	board, err := s.roster.Board(r.Context(), path)
	s.rosterResult(w, board, err)
}

func (s *Server) rosterCreate(w http.ResponseWriter, r *http.Request) {
	path, ok := s.rosterGuard(w, r)
	if !ok {
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
	result, err := s.roster.Create(path, req.Name, req.Account, req.Browser)
	s.rosterResult(w, result, err)
}

func (s *Server) rosterCopy(w http.ResponseWriter, r *http.Request) {
	path, ok := s.rosterGuard(w, r)
	if !ok {
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
	result, err := s.roster.Copy(r.Context(), path, req.From, req.To, req.Domain, req.Mode)
	s.rosterResult(w, result, err)
}

func (s *Server) rosterPin(w http.ResponseWriter, r *http.Request) {
	path, ok := s.rosterGuard(w, r)
	if !ok {
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
	err := s.roster.Pin(path, req.Profile, req.Origin, req.Account, req.Label)
	s.rosterResult(w, map[string]any{"ok": err == nil}, err)
}

func (s *Server) rosterOpen(w http.ResponseWriter, r *http.Request) {
	path, ok := s.rosterGuard(w, r)
	if !ok {
		return
	}
	var req struct {
		Profile string `json:"profile"`
		URL     string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.roster.Open(r.Context(), path, req.Profile, req.URL)
	s.rosterResult(w, result, err)
}
