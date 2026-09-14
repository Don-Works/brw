package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Don-Works/brw/internal/plugin"
)

// SetPluginRegistry installs the plugins this daemon loaded. A daemon started
// without --plugin-dir gets an empty registry rather than nil, so the routes
// answer "nothing loaded" instead of "not configured".
func (s *Server) SetPluginRegistry(registry *plugin.Registry) { s.plugins = registry }

// errNoPluginRegistry names the one case the routes cannot answer: an embedder
// that built a Server and never called SetPluginRegistry. brwd always does.
var errNoPluginRegistry = errors.New("plugin registry is not configured on this daemon")

// listPlugins reports what is loaded and what each holds. It deliberately names
// the credential backend KIND but not its argv or directory: the daemon's own
// filesystem layout is not part of a control-plane answer.
func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, errNoPluginRegistry)
		return
	}
	_ = r
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins":                s.plugins.Plugins(),
		"grantable_capabilities": plugin.GrantableCapabilities(),
	})
}

// revokePlugin drops a plugin's grants for the life of this process.
//
// There is deliberately no grant route to match it. Revoking NARROWS what brw
// can do and fails closed, which makes it safe for anything that can reach the
// control plane; granting widens it and stays an operator action against a
// filesystem the daemon checked the permissions of at startup.
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
