package httpapi

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/browser"
)

// errSessionStateUnavailable is what a controller with no session-state
// capability answers over HTTP. It mirrors the MCP wording so an operator
// reading a daemon log and an agent reading a tool error see the same reason.
var errSessionStateUnavailable = errors.New("this browser transport does not support session snapshots: brw_state reads and writes cookies at the browser-context level, which needs a direct-CDP profile")

// sessionState is the browser host's end of brw_state. The request carries the
// action, the origin allowlist and an opaque snapshot id; the response carries
// metadata and counts. No cookie ever crosses this route in either direction,
// which is what lets an --upstream-http daemon offer brw_state without the
// session material leaving the browser host.
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
