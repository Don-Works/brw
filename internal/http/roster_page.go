package httpapi

import (
	_ "embed"
	"net/http"
)

//go:embed roster.html
var rosterHTML []byte

//go:embed roster.js
var rosterJS []byte

func init() {
	rosterPageHTML = rosterHTML
}

func (s *Server) rosterScript(w http.ResponseWriter, r *http.Request) {
	if !s.rosterGuard(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(rosterJS)
}
