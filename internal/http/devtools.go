package httpapi

import (
	"net/http"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/devtools"
)

func (s *Server) devtoolsObserver(w http.ResponseWriter) (devtools.Observer, bool) {
	observer, ok := s.manager.(devtools.Observer)
	if !ok {
		writeError(w, devtools.ErrUnsupported)
		return nil, false
	}
	return observer, true
}

func (s *Server) vitals(w http.ResponseWriter, r *http.Request) {
	observer, ok := s.devtoolsObserver(w)
	if !ok {
		return
	}
	var req devtools.VitalsOptions
	if !decode(w, r, &req) {
		return
	}
	result, err := observer.Vitals(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) accessibilityAudit(w http.ResponseWriter, r *http.Request) {
	observer, ok := s.devtoolsObserver(w)
	if !ok {
		return
	}
	var req devtools.AuditOptions
	if !decode(w, r, &req) {
		return
	}
	ctx := s.contextWithTabID(r.Context(), req.TabID)
	result, err := observer.AccessibilityAudit(ctx, req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, artifact.AttachAuditReport(ctx, s.artifacts, result, req.ReportTTL()), nil)
}

func (s *Server) highlight(w http.ResponseWriter, r *http.Request) {
	observer, ok := s.devtoolsObserver(w)
	if !ok {
		return
	}
	var req devtools.HighlightOptions
	if !decode(w, r, &req) {
		return
	}
	result, err := observer.Highlight(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}
