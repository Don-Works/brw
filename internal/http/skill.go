package httpapi

import (
	"net/http"

	"github.com/Don-Works/brw/internal/agentskill"
)

// SetVersion records the build this daemon is, so /api/skill can stamp the manual it serves with the version of the binary that served it.
func (s *Server) SetVersion(version string) { s.version = version }

func (s *Server) agentSkill(w http.ResponseWriter, r *http.Request) {
	document, err := agentskill.Read(r.URL.Query().Get("document"), s.version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}
