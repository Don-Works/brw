package httpapi

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/pagewatch"
)

// SetPageWatchAPI installs durable browser-host observation registration.
func (s *Server) SetPageWatchAPI(api pagewatch.API) { s.pageWatch = api }

func (s *Server) pageWatchService() pagewatch.API {
	if api, ok := s.manager.(pagewatch.API); ok {
		return api
	}
	return s.pageWatch
}

func (s *Server) watchPage(w http.ResponseWriter, r *http.Request) {
	var req pagewatch.RegisterOptions
	if !decodeStrict(w, r, &req) {
		return
	}
	api := s.pageWatchService()
	if api == nil {
		writeError(w, errors.New("persistent page watchers are disabled"))
		return
	}
	out, err := api.WatchPage(r.Context(), req)
	writeResult(w, out, err)
}

func (s *Server) pageWatchers(w http.ResponseWriter, r *http.Request) {
	var req pagewatch.ManageOptions
	if !decodeStrict(w, r, &req) {
		return
	}
	api := s.pageWatchService()
	if api == nil {
		writeError(w, errors.New("persistent page watchers are disabled"))
		return
	}
	out, err := api.PageWatchers(r.Context(), req)
	writeResult(w, out, err)
}

func (s *Server) pageEvents(w http.ResponseWriter, r *http.Request) {
	var req pagewatch.EventsOptions
	if !decodeStrict(w, r, &req) {
		return
	}
	api := s.pageWatchService()
	if api == nil {
		writeError(w, errors.New("persistent page watchers are disabled"))
		return
	}
	out, err := api.PageEvents(r.Context(), req)
	writeResult(w, out, err)
}
