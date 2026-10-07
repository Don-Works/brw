package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Don-Works/brw/internal/plugin"
)

// SetPluginRegistry installs the plugins this daemon loaded.
func (s *Server) SetPluginRegistry(registry *plugin.Registry) { s.plugins = registry }

var errNoPluginRegistry = errors.New("plugin registry is not configured on this daemon")

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, errNoPluginRegistry)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins":                s.plugins.Plugins(),
		"grantable_capabilities": plugin.GrantableCapabilities(),
	})
}

func (s *Server) revokePlugin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, errors.New("plugin id is required"))
		return
	}
	if s.plugins == nil {
		writeError(w, errNoPluginRegistry)
		return
	}
	if err := s.plugins.Revoke(req.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": req.ID, "plugins": s.plugins.Plugins()})
}
