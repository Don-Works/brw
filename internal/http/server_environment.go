package httpapi

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/browser"
)

// environmentController resolves the optional page-environment capability once
// per request. A transport that cannot hold these overrides answers with the
// named capability error rather than a generic 400, so a caller can tell "wrong
// transport" from "bad arguments".
func (s *Server) environmentController(w http.ResponseWriter) (browser.EnvironmentController, bool) {
	env, ok := s.manager.(browser.EnvironmentController)
	if !ok {
		writeError(w, browser.ErrEnvironmentUnsupported)
		return nil, false
	}
	return env, true
}

func (s *Server) geolocation(w http.ResponseWriter, r *http.Request) {
	var req browser.GeolocationOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.SetGeolocation(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) networkConditions(w http.ResponseWriter, r *http.Request) {
	var req browser.NetworkConditionsOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.SetNetworkConditions(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) emulateMedia(w http.ResponseWriter, r *http.Request) {
	var req browser.MediaEmulationOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.EmulateMedia(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) extraHeaders(w http.ResponseWriter, r *http.Request) {
	var req browser.ExtraHeadersOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.SetExtraHeaders(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

func (s *Server) userAgent(w http.ResponseWriter, r *http.Request) {
	var req browser.UserAgentOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.SetUserAgent(s.contextWithTabID(r.Context(), req.TabID), req)
	writeResult(w, result, err)
}

// authenticate takes a password in its request body. It is the one page route
// whose body must never be echoed: decode errors are reported as a constant
// string rather than the decoder's message, which quotes the offending JSON.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	var req browser.CredentialsOptions
	if !decodeQuiet(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	// Marked sensitive here as well as in the MCP layer: with --upstream-http the
	// MCP wrapper and the daemon holding the trace are different processes, so a
	// mark applied only there never reaches the trace this navigation writes.
	ctx := browser.WithSensitiveAction(s.contextWithTabID(r.Context(), req.TabID))
	result, err := env.Authenticate(ctx, req)
	writeResult(w, result, err)
}

func (s *Server) downloadPath(w http.ResponseWriter, r *http.Request) {
	var req browser.DownloadPathOptions
	if !decode(w, r, &req) {
		return
	}
	env, ok := s.environmentController(w)
	if !ok {
		return
	}
	result, err := env.SetDownloadPath(r.Context(), req)
	writeResult(w, result, err)
}

// decodeQuiet is decode with the decoder's message withheld. json's errors quote
// the input they choked on, which for a credential body is the credential.
func decodeQuiet(w http.ResponseWriter, r *http.Request, dst any) bool {
	recorder := &quietResponse{ResponseWriter: w}
	if decode(recorder, r, dst) {
		return true
	}
	writeError(w, errors.New("request body is not valid JSON for this endpoint"))
	return false
}

// quietResponse swallows the body decode writes so the caller can substitute
// its own. Status and headers are dropped with it; the substitute writes both.
type quietResponse struct {
	http.ResponseWriter
}

func (q *quietResponse) Write(p []byte) (int, error) { return len(p), nil }

func (q *quietResponse) WriteHeader(int) {}
