//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/plugin"
)

// Obviously fabricated, low entropy, and shaped like the two things a websocket
// endpoint hides: the path Chrome authenticates the socket with, and the key a
// hosted provider carries in the query.
const (
	fixtureSessionPath = "devtools-session-fixture"
	fixtureProviderKey = "fixture-provider-key-two"
)

func standInPluginDir(t *testing.T) *plugin.Registry {
	t.Helper()
	scripts := t.TempDir()
	root := t.TempDir()
	envelope := `{"websocket_url":"wss://browsers.example/` + fixtureSessionPath +
		`?token=` + fixtureProviderKey + `","session_id":"sess-7","expires_in_ms":600000}`
	mint := filepath.Join(scripts, "mint")
	if err := os.WriteFile(mint, []byte("#!/bin/sh\ncat <<'EOF'\n"+envelope+"\nEOF\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	teardown := filepath.Join(scripts, "teardown")
	if err := os.WriteFile(teardown, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{mint, teardown} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(map[string]any{
		"schema_version": plugin.ManifestSchemaVersion,
		"id":             "local.standin",
		"name":           "Local stand-in browser",
		"version":        "1.0.0",
		"description":    "Mints a CDP endpoint",
		"capabilities":   []string{plugin.CapabilityBrowserProvider},
		"browser": map[string]any{
			"kind":     plugin.BrowserKindExec,
			"command":  []string{mint},
			"teardown": []string{teardown, plugin.SessionToken},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "standin.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugin.Load(root)
	if err != nil {
		t.Fatalf("load the plugin directory: %v", err)
	}
	return registry
}

// Acceptance 4, at the call site. The daemon logs what it is driving, and the
// line it writes is the one that would put a provider's session URL into a log
// file on disk. Asserting the type redacts is not the same as asserting this
// call site uses the redacting form, which is the mistake that ships.
func TestTheDaemonLogsTheProviderSessionWithoutItsCredentials(t *testing.T) {
	registry := standInPluginDir(t)

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	target, release, err := openProviderBrowser(context.Background(), registry)
	if err != nil {
		t.Fatalf("openProviderBrowser: %v", err)
	}
	t.Cleanup(func() { _ = release(context.Background()) })

	line := logged.String()
	if line == "" {
		t.Fatal("the daemon logged nothing about the browser it is driving")
	}
	for _, secret := range []string{fixtureSessionPath, fixtureProviderKey} {
		if strings.Contains(line, secret) {
			t.Errorf("the startup log line %q carries %q, which authenticates the session", line, secret)
		}
	}
	// It still has to say enough to be worth logging.
	for _, want := range []string{"local.standin", "sess-7", "browsers.example"} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup log line %q does not name %q", line, want)
		}
	}
	// And the dialable form is carried to the manager, not lost to redaction.
	if !strings.Contains(target.WebSocketURL, fixtureSessionPath) || !strings.Contains(target.WebSocketURL, fixtureProviderKey) {
		t.Fatalf("the remote target lost the URL it has to dial: %q", target.RedactedURL)
	}
	if strings.Contains(target.RedactedURL, fixtureSessionPath) || strings.Contains(target.RedactedURL, fixtureProviderKey) {
		t.Fatalf("the reportable endpoint %q carries the part that authenticates it", target.RedactedURL)
	}
	if target.ExpiresAt.IsZero() {
		t.Fatal("the provider's stated lifetime did not reach the manager, so nothing can report the session as expired")
	}
}
