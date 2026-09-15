package httpapi

import (
	"net/http"

	"github.com/Don-Works/brw/internal/agentskill"
)

// SetVersion records the build this daemon is, so /api/skill can stamp the
// manual it serves with the version of the binary that served it. Empty (the
// default) is reported verbatim rather than guessed at: a caller comparing a
// page against a build needs to know when nobody said which build this is.
func (s *Server) SetVersion(version string) { s.version = version }

// agentSkill serves brw's own operating manual out of this binary.
//
// It is the HTTP half of the same answer brw_skill gives over MCP, and it reads
// the embedded copy for the same reason: a page on disk was written by whichever
// brw was installed when `brwctl setup` last ran, and after an upgrade it
// describes a surface this daemon no longer has. Nothing on this path touches
// the filesystem, so nothing on disk can shadow it.
func (s *Server) agentSkill(w http.ResponseWriter, r *http.Request) {
	document, err := agentskill.Read(r.URL.Query().Get("document"), s.version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}
