package httpapi

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/browser"
)

var errSessionStateUnavailable = errors.New("this browser transport does not support session snapshots: brw_state reads and writes cookies at the browser-context level, which needs a direct-CDP profile")

func (s *Server) sessionState(w http.ResponseWriter, r *http.Request) {
	state, ok := s.manager.(browser.SessionStateController)
	if !ok {
		writeError(w, errSessionStateUnavailable)
		return
	}
	var req browser.SessionStateOptions
	if !decodeStrict(w, r, &req) {
		return
	}
	result, err := state.SessionState(r.Context(), req)
	writeResult(w, result, err)
}
