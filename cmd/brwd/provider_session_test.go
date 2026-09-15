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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
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
	return standInPluginDirWithEndpoint(t, "wss://browsers.example/"+fixtureSessionPath+`?token=`+fixtureProviderKey)
}

func standInPluginDirWithEndpoint(t *testing.T, websocketURL string) *plugin.Registry {
	t.Helper()
	scripts := t.TempDir()
	root := t.TempDir()
	envelope := `{"websocket_url":"` + websocketURL + `","session_id":"sess-7","expires_in_ms":600000}`
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

// brw shouts about --ignore-https-errors and about --unsafe-real-profile. A
// provider handing back a plaintext ws:// endpoint to another machine is the
// same class of fact — every byte of the CDP session, which is page content,
// the cookies brw_cookies reads and the text brw_fill types, crosses the
// network in the clear — and it was the one accepted in silence.
func TestAPlaintextEndpointToAnotherHostIsWarnedAbout(t *testing.T) {
	for name, test := range map[string]struct {
		endpoint string
		wantWarn bool
	}{
		"plaintext to another host": {"ws://browsers.example/devtools-session-fixture", true},
		"plaintext by ip":           {"ws://198.51.100.7:9222/devtools-session-fixture", true},
		"encrypted":                 {"wss://browsers.example/devtools-session-fixture", false},
		"loopback stand-in":         {"ws://127.0.0.1:9222/devtools-session-fixture", false},
		"loopback by name":          {"ws://localhost:9222/devtools-session-fixture", false},
		"loopback v6":               {"ws://[::1]:9222/devtools-session-fixture", false},
	} {
		t.Run(name, func(t *testing.T) {
			registry := standInPluginDirWithEndpoint(t, test.endpoint)
			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(previous) })

			_, release, err := openProviderBrowser(context.Background(), registry)
			if err != nil {
				t.Fatalf("openProviderBrowser: %v", err)
			}
			t.Cleanup(func() { _ = release(context.Background()) })

			warned := strings.Contains(logged.String(), "WARNING") && strings.Contains(logged.String(), "unencrypted")
			if warned != test.wantWarn {
				t.Fatalf("endpoint %q warned=%v, want %v; log was %q", test.endpoint, warned, test.wantWarn, logged.String())
			}
			// Warned or not, the part that authenticates the socket never
			// reaches a log line.
			if strings.Contains(logged.String(), "devtools-session-fixture") {
				t.Fatalf("the log carries the path that authenticates the session: %q", logged.String())
			}
		})
	}
}

// A provider session is given back exactly once per open. Manager.Close
// releases it on a failed connect and clears the field; a caller that released
// again would run the operator's teardown twice, resolve the provider
// credential twice more, and log the second exit status as a failed release —
// which reads as a leaked session when nothing leaked.
func TestAFailedConnectGivesTheBrowserBackExactlyOnce(t *testing.T) {
	var releases atomic.Int64
	// Port 1 on loopback: nothing listens, so the CDP dial fails immediately.
	remote := &browser.RemoteTarget{
		WebSocketURL: "ws://127.0.0.1:1/devtools/browser/fixture-session",
		RedactedURL:  "ws://127.0.0.1:1",
		ProviderID:   "local.standin",
		SessionID:    "sess-7",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	release := func(context.Context) error {
		releases.Add(1)
		return nil
	}
	remote.Release = release

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := browser.New(ctx, browser.Config{Timeout: 5 * time.Second, Remote: remote}); err == nil {
		t.Fatal("connecting to a dead endpoint succeeded")
	}
	releaseUnreleasedSession(remote, release)
	if got := releases.Load(); got != 1 {
		t.Fatalf("the provider session was released %d times, want exactly 1", got)
	}

	// And the case the guard must NOT skip: a configuration refused before the
	// connect leaves the session held, so it still has to be given back.
	var refusedReleases atomic.Int64
	refusedRelease := func(context.Context) error {
		refusedReleases.Add(1)
		return nil
	}
	held := &browser.RemoteTarget{
		WebSocketURL: "ws://127.0.0.1:1/devtools/browser/fixture-session",
		RedactedURL:  "ws://127.0.0.1:1",
		Release:      refusedRelease,
	}
	if _, err := browser.New(ctx, browser.Config{Timeout: 5 * time.Second, Remote: held, UserDataDir: "/tmp/fixture-profile"}); err == nil {
		t.Fatal("a provider launch with a profile directory was accepted")
	}
	releaseUnreleasedSession(held, refusedRelease)
	if got := refusedReleases.Load(); got != 1 {
		t.Fatalf("a session refused before the connect was released %d times, want exactly 1", got)
	}
}
