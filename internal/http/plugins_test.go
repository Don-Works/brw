package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/plugin"
)

// loopbackRequest builds a request the control-plane host guard accepts: the
// guard exists to stop a rebound DNS name reaching the daemon, and httptest's
// default Host is example.com.
func loopbackRequest(method, path, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "127.0.0.1:17310"
	return request
}

func pluginRegistryForRoutes(t *testing.T) *plugin.Registry {
	t.Helper()
	root, credentials := t.TempDir(), t.TempDir()
	manifest, err := json.Marshal(map[string]any{
		"schema_version": plugin.ManifestSchemaVersion,
		"id":             "local.files",
		"name":           "Local credential files",
		"version":        "1.0.0",
		"description":    "One file per credential",
		"capabilities":   []string{plugin.CapabilityCredentialRead},
		"credential":     map[string]any{"kind": plugin.CredentialKindFile, "directory": credentials},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugin.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestPluginRoutesListAndRevoke(t *testing.T) {
	server := New("127.0.0.1:17310", nonStreamingController{})
	registry := pluginRegistryForRoutes(t)
	server.SetPluginRegistry(registry)

	listed := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(listed, loopbackRequest(http.MethodGet, "/api/plugins", ""))
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /api/plugins = %d: %s", listed.Code, listed.Body)
	}
	if !strings.Contains(listed.Body.String(), "local.files") ||
		!strings.Contains(listed.Body.String(), plugin.CapabilityCredentialRead) {
		t.Fatalf("plugin listing = %s", listed.Body)
	}

	revoked := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(revoked, loopbackRequest(http.MethodPost, "/api/plugins/revoke", `{"id":"local.files"}`))
	if revoked.Code != http.StatusOK {
		t.Fatalf("POST /api/plugins/revoke = %d: %s", revoked.Code, revoked.Body)
	}
	if err := registry.ProbeProvider(); err == nil {
		t.Fatal("the registry still holds a provider after the route revoked it")
	}

	missing := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(missing, loopbackRequest(http.MethodPost, "/api/plugins/revoke", `{"id":"absent.plugin"}`))
	if missing.Code == http.StatusOK {
		t.Fatalf("revoking an unknown plugin answered %d: %s", missing.Code, missing.Body)
	}
}

// There is no grant route, and there must not be one: granting widens what brw
// can do, and a control plane an agent can reach is not where that decision
// belongs. This reads the daemon's own registration table so adding one is a
// failing test rather than a review comment nobody makes.
func TestThereIsNoRouteThatGrantsAPluginCapability(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/api/plugins/grant", "/api/plugins/load", "/api/plugins/install", "/api/plugins/reload"} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("the daemon registers %s; loading and granting a plugin is a filesystem action, not a control-plane one", forbidden)
		}
	}
}

func TestPluginRoutesAnswerWhenNoRegistryIsInstalled(t *testing.T) {
	server := New("127.0.0.1:17310", nonStreamingController{})
	recorder := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(recorder, loopbackRequest(http.MethodGet, "/api/plugins", ""))
	if recorder.Code == http.StatusOK {
		t.Fatalf("a daemon with no registry answered the listing with %d: %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "not configured") {
		t.Fatalf("listing without a registry = %s, want a named refusal", recorder.Body)
	}
}
