//go:build !windows

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/credential"
)

// Obviously fabricated and low entropy, like every other fixture secret here.
const fixtureProviderKey = "fixture-provider-key-two"

// writeScript writes an executable shell program. Chmodded after the write
// because the process umask clears the bits the trust checks are about.
func writeScript(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// BrowserKinds is the closed domain of provider backends. Every member has to
// be constructible and every non-member refused, or the switch in
// newBrowserProvider is a gate with a hole where a sibling kind should be.
func TestEveryBrowserKindIsConstructibleAndNothingElseIs(t *testing.T) {
	dir := t.TempDir()
	mint := writeScript(t, filepath.Join(dir, "mint"), "echo '{}'")
	for _, kind := range BrowserKinds {
		t.Run(kind, func(t *testing.T) {
			spec := BrowserProviderSpec{Kind: kind, Command: []string{mint}, Teardown: []string{mint, SessionToken}}
			if err := validateBrowserSpec(spec); err != nil {
				t.Fatalf("declared kind %q does not validate: %v", kind, err)
			}
			provider, err := newBrowserProvider(spec)
			if err != nil {
				t.Fatalf("declared kind %q cannot be constructed: %v", kind, err)
			}
			if provider == nil {
				t.Fatalf("declared kind %q built a nil provider", kind)
			}
		})
	}
	for _, kind := range []string{"", "Exec", "exec ", "file", "browserbase", "http"} {
		spec := BrowserProviderSpec{Kind: kind, Command: []string{mint}, Teardown: []string{mint, SessionToken}}
		if err := validateBrowserSpec(spec); err == nil {
			t.Errorf("browser kind %q validated; it is not in %v", kind, BrowserKinds)
		}
		if _, err := newBrowserProvider(spec); err == nil {
			t.Errorf("browser kind %q was constructed; it is not in %v", kind, BrowserKinds)
		}
	}
}

func TestBrowserManifestValidation(t *testing.T) {
	dir := t.TempDir()
	program := writeScript(t, filepath.Join(dir, "p"), "echo '{}'")
	for name, test := range map[string]struct {
		mutate  func(spec *BrowserProviderSpec)
		wantErr string
	}{
		"valid":               {func(*BrowserProviderSpec) {}, ""},
		"no command":          {func(s *BrowserProviderSpec) { s.Command = nil }, "requires a browser command"},
		"no teardown":         {func(s *BrowserProviderSpec) { s.Teardown = nil }, "requires a browser teardown"},
		"relative program":    {func(s *BrowserProviderSpec) { s.Command = []string{"mint"} }, "must be an absolute path"},
		"unclean program":     {func(s *BrowserProviderSpec) { s.Command = []string{"/usr/bin/../bin/echo"} }, "clean path"},
		"teardown no session": {func(s *BrowserProviderSpec) { s.Teardown = []string{program} }, "exactly 1 times"},
		"teardown two sessions": {
			func(s *BrowserProviderSpec) { s.Teardown = []string{program, SessionToken, SessionToken} },
			"exactly 1 times",
		},
		"mint carries a session token": {
			func(s *BrowserProviderSpec) { s.Command = []string{program, SessionToken} },
			"exactly 0 times",
		},
		"reference token in the argv": {
			func(s *BrowserProviderSpec) { s.Command = []string{program, ReferenceToken} },
			"receives its credential on stdin",
		},
		"reference token in the program": {
			func(s *BrowserProviderSpec) { s.Command = []string{"/bin/" + ReferenceToken} },
			"must not contain",
		},
		"bad credential reference": {
			func(s *BrowserProviderSpec) { s.Credential = "../../etc/shadow" },
			"credential reference",
		},
		"timeout too long": {func(s *BrowserProviderSpec) { s.TimeoutMS = maxBrowserTimeoutMS + 1 }, "timeout_ms"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := BrowserProviderSpec{Kind: BrowserKindExec, Command: []string{program}, Teardown: []string{program, SessionToken}}
			test.mutate(&spec)
			err := validateBrowserSpec(spec)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateBrowserSpec = %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateBrowserSpec = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
}

// A provider's answer is attacker-shaped input as far as brw is concerned: it
// arrives as bytes on a pipe and is turned into a URL brw dials and an argv brw
// execs. Every field is checked.
func TestSessionEnvelopeRefusesEverythingButAWellFormedAnswer(t *testing.T) {
	const ok = `{"websocket_url":"ws://127.0.0.1:9222/devtools/browser/abc","session_id":"s-1","expires_in_ms":600000}`
	for name, test := range map[string]struct {
		raw     string
		wantErr string
	}{
		"valid":            {ok, ""},
		"empty":            {``, "valid session envelope"},
		"not json":         {`nope`, "valid session envelope"},
		"unknown field":    {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":1000,"extra":1}`, "valid session envelope"},
		"trailing json":    {ok + `{"x":1}`, "trailing output"},
		"http scheme":      {`{"websocket_url":"http://127.0.0.1:9222/json","session_id":"s","expires_in_ms":1000}`, "must be ws or wss"},
		"file scheme":      {`{"websocket_url":"file:///etc/passwd","session_id":"s","expires_in_ms":1000}`, "must be ws or wss"},
		"no host":          {`{"websocket_url":"ws:///devtools","session_id":"s","expires_in_ms":1000}`, "no host"},
		"userinfo":         {`{"websocket_url":"wss://key:tok@h/p","session_id":"s","expires_in_ms":1000}`, "userinfo"},
		"empty url":        {`{"websocket_url":"","session_id":"s","expires_in_ms":1000}`, "empty websocket URL"},
		"bad session id":   {`{"websocket_url":"ws://h/p","session_id":"s;rm -rf /","expires_in_ms":1000}`, "session_id"},
		"no session id":    {`{"websocket_url":"ws://h/p","session_id":"","expires_in_ms":1000}`, "session_id"},
		"no lifetime":      {`{"websocket_url":"ws://h/p","session_id":"s"}`, "must state expires_in_ms"},
		"negative":         {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":-5}`, "must state expires_in_ms"},
		"lifetime too big": {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":86400001}`, "lifetime over"},
		"lifetime tiny":    {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":5}`, "lifetime under"},
	} {
		t.Run(name, func(t *testing.T) {
			session, err := ParseSessionEnvelope([]byte(test.raw))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseSessionEnvelope = %v, want it accepted", err)
				}
				if session.Endpoint.Empty() || session.SessionID == "" || session.Lifetime == 0 {
					t.Fatalf("accepted envelope produced %+v", session)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseSessionEnvelope(%q) = %v, want an error containing %q", test.raw, err, test.wantErr)
			}
		})
	}
}

// The path of a CDP websocket URL authenticates the connection: Chrome's
// /devtools/browser/<uuid> is the whole credential for that browser, and a
// hosted provider puts its key in the query. Every rendering path must redact,
// because "we remembered not to log it" is not a property anything can check.
func TestEndpointRedactsOnEveryRenderingPath(t *testing.T) {
	// Both halves an endpoint hides: the path Chrome authenticates with, and
	// the query a hosted provider carries its key in.
	const secretPath = "devtools-session-fixture"
	const endpointURL = "wss://browsers.example/devtools-session-fixture?token=fixture-provider-key-two"
	endpoint, err := ParseEndpoint(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpointURL, secretPath) || !strings.Contains(endpointURL, fixtureProviderKey) {
		t.Fatal("the fixture URL must carry both secrets, or this test proves nothing")
	}
	// %s is spelled through a slice so the vet/staticcheck "just call String()"
	// rewrite does not turn the test into a check that String() equals itself.
	// The point is what fmt does with the value, not what String returns.
	viaFmtS := fmt.Sprintf("%s", []any{endpoint}...)
	renderings := map[string]string{
		"String":  endpoint.String(),
		"%v":      fmt.Sprintf("%v", endpoint),
		"%s":      viaFmtS,
		"%q":      fmt.Sprintf("%q", endpoint),
		"%#v":     fmt.Sprintf("%#v", endpoint),
		"in-err":  fmt.Errorf("dial %v failed", endpoint).Error(),
		"struct":  fmt.Sprintf("%v", BrowserSession{Endpoint: endpoint}),
		"structv": fmt.Sprintf("%+v", BrowserSession{Endpoint: endpoint}),
	}
	for name, rendered := range renderings {
		if strings.Contains(rendered, secretPath) || strings.Contains(rendered, fixtureProviderKey) {
			t.Errorf("%s rendered the endpoint as %q, which carries the part that authenticates it", name, rendered)
		}
		if !strings.Contains(rendered, "browsers.example") {
			t.Errorf("%s rendered the endpoint as %q; the host has to survive or the message names nothing", name, rendered)
		}
	}
	encoded, err := endpoint.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretPath) || strings.Contains(string(encoded), fixtureProviderKey) {
		t.Errorf("marshalled endpoint = %s", encoded)
	}
	// And the one caller allowed to use it still gets the whole thing.
	if !strings.Contains(endpoint.Reveal(), secretPath) {
		t.Fatal("Reveal must return the dialable URL")
	}
}

// providerRegistry builds a loaded registry whose browser provider is a shell
// script, plus the file credential provider when reference is non-empty.
func providerRegistry(t *testing.T, mintBody, teardownBody, reference, value string) (*Registry, string) {
	t.Helper()
	scripts := t.TempDir()
	root := t.TempDir()
	// Where the teardown records what it was handed, so a test can read it back.
	record := filepath.Join(scripts, "teardown.log")
	mint := writeScript(t, filepath.Join(scripts, "mint"), mintBody)
	teardown := writeScript(t, filepath.Join(scripts, "teardown"), teardownBody)
	manifest := browserManifest(t, mint, teardown)
	if reference != "" {
		credentials := t.TempDir()
		path := filepath.Join(credentials, filepath.FromSlash(reference))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest["capabilities"] = []string{CapabilityBrowserProvider, CapabilityCredentialRead}
		manifest["credential"] = map[string]any{"kind": CredentialKindFile, "directory": credentials}
		browser, _ := manifest["browser"].(map[string]any)
		browser["credential"] = reference
	}
	writeManifest(t, root, "browser.json", manifest)
	registry, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return registry, record
}

const goodEnvelope = `{"websocket_url":"ws://127.0.0.1:9222/devtools/browser/fixture","session_id":"sess-42","expires_in_ms":600000}`

func TestOpenBrowserSessionMintsAndReleases(t *testing.T) {
	registry, record := providerRegistry(t,
		"echo '"+goodEnvelope+"'",
		`printf '%s' "$1" > `+"$(dirname \"$0\")/teardown.log", "", "")
	ctx := context.Background()
	session, release, err := registry.OpenBrowserSession(ctx)
	if err != nil {
		t.Fatalf("OpenBrowserSession: %v", err)
	}
	if session.SessionID != "sess-42" {
		t.Fatalf("session id = %q", session.SessionID)
	}
	if session.ProviderID != "local.standin" {
		t.Fatalf("provider id = %q; a failure has to name which plugin answered", session.ProviderID)
	}
	if session.Lifetime != 10*time.Minute {
		t.Fatalf("lifetime = %s", session.Lifetime)
	}
	if got := session.Endpoint.Reveal(); got != "ws://127.0.0.1:9222/devtools/browser/fixture" {
		t.Fatalf("endpoint = %q", got)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The teardown has to be aimed at the session that was opened, not at
	// whatever the provider considers current.
	handed, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("teardown recorded nothing: %v", err)
	}
	if string(handed) != "sess-42" {
		t.Fatalf("teardown was handed %q, want the opened session id", handed)
	}
}

// Criterion 4: a provider credential is passed by reference. brw resolves the
// reference through the credential.read holder and hands the VALUE to the
// provider on stdin — never in the argv, which every process on the machine can
// read out of /proc or ps.
func TestProviderCredentialTravelsOnStdinAndNeverInTheArgv(t *testing.T) {
	// The mint script proves both halves at once: it reads the key from stdin
	// and prints its own argv into the envelope's session id slot, so an argv
	// carrying the key would fail the session-id pattern AND show up here.
	registry, _ := providerRegistry(t,
		`read key
echo "{\"websocket_url\":\"ws://127.0.0.1:9222/devtools/browser/$key\",\"session_id\":\"sess-1\",\"expires_in_ms\":600000}"
echo "argv:$*" >&2`,
		`read key
printf '%s' "$key" > "$(dirname "$0")/teardown.log"`,
		"work/provider/key", fixtureProviderKey)
	ctx := context.Background()
	session, release, err := registry.OpenBrowserSession(ctx)
	if err != nil {
		t.Fatalf("OpenBrowserSession: %v", err)
	}
	if !strings.HasSuffix(session.Endpoint.Reveal(), "/"+fixtureProviderKey) {
		t.Fatalf("the provider did not receive the credential on stdin; endpoint path = %q", session.Endpoint.Reveal())
	}
	// Redaction still holds for an endpoint that now literally contains the key.
	if rendered := fmt.Sprintf("%v", session.Endpoint); strings.Contains(rendered, fixtureProviderKey) {
		t.Fatalf("endpoint rendered as %q, exposing the provider credential", rendered)
	}
	// And the teardown gets it the same way, resolved fresh rather than from a
	// value brw held onto for the life of the session.
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestBrowserProviderFailuresAreAllClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("no provider", func(t *testing.T) {
		if _, _, err := Empty().OpenBrowserSession(ctx); !errors.Is(err, ErrNoBrowserProvider) {
			t.Fatalf("OpenBrowserSession with nothing loaded = %v, want ErrNoBrowserProvider", err)
		}
		if err := Empty().ProbeBrowserProvider(); !errors.Is(err, ErrNoBrowserProvider) {
			t.Fatalf("ProbeBrowserProvider = %v", err)
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		var registry *Registry
		if _, _, err := registry.OpenBrowserSession(ctx); !errors.Is(err, ErrNoBrowserProvider) {
			t.Fatalf("nil registry = %v", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		registry, _ := providerRegistry(t, "echo '"+goodEnvelope+"'", "true", "", "")
		if err := registry.Revoke("local.standin"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := registry.OpenBrowserSession(ctx); !errors.Is(err, ErrBrowserProviderRevoked) {
			t.Fatalf("revoked provider = %v, want ErrBrowserProviderRevoked", err)
		}
	})
	t.Run("provider exits non-zero", func(t *testing.T) {
		registry, _ := providerRegistry(t, "exit 3", "true", "", "")
		_, _, err := registry.OpenBrowserSession(ctx)
		if err == nil || !strings.Contains(err.Error(), "exited with status 3") {
			t.Fatalf("failing provider = %v", err)
		}
	})
	t.Run("provider prints nonsense", func(t *testing.T) {
		registry, _ := providerRegistry(t, "echo not-json", "true", "", "")
		_, _, err := registry.OpenBrowserSession(ctx)
		if err == nil || !strings.Contains(err.Error(), "session envelope") {
			t.Fatalf("nonsense provider = %v", err)
		}
	})
	t.Run("credential reference cannot be resolved", func(t *testing.T) {
		// The browser plugin declares a reference and no plugin holds
		// credential.read, so there is nothing to resolve it. Failing closed
		// here is what stops brw minting an unauthenticated session instead.
		scripts := t.TempDir()
		root := t.TempDir()
		mint := writeScript(t, filepath.Join(scripts, "mint"), "echo '"+goodEnvelope+"'")
		manifest := browserManifest(t, mint, mint)
		browser, _ := manifest["browser"].(map[string]any)
		browser["credential"] = "work/provider/key"
		writeManifest(t, root, "browser.json", manifest)
		registry, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = registry.OpenBrowserSession(ctx)
		if !errors.Is(err, credential.ErrNoProvider) {
			t.Fatalf("unresolvable provider credential = %v, want the no-provider refusal", err)
		}
	})
}

// A session already open has to be returnable. Revoking the plugin narrows what
// brw will do NEXT; if it also broke the teardown, every revoke would leak the
// cloud browser (and its bill) that was running at the time.
func TestRevokeStopsTheNextOpenAndStillAllowsTheRelease(t *testing.T) {
	registry, record := providerRegistry(t,
		"echo '"+goodEnvelope+"'",
		`printf '%s' "$1" > "$(dirname "$0")/teardown.log"`, "", "")
	ctx := context.Background()
	_, release, err := registry.OpenBrowserSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Revoke("local.standin"); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release after revoke = %v; a revoked plugin must still take its browser back", err)
	}
	if _, err := os.ReadFile(record); err != nil {
		t.Fatalf("teardown did not run after revoke: %v", err)
	}
	if _, _, err := registry.OpenBrowserSession(ctx); !errors.Is(err, ErrBrowserProviderRevoked) {
		t.Fatalf("open after revoke = %v", err)
	}
}

// The plugins listing is what an operator reads to see what a daemon holds. A
// browser backend that did not appear there would be a grant nobody can audit.
func TestStatusNamesTheBrowserBackend(t *testing.T) {
	registry, _ := providerRegistry(t, "echo '"+goodEnvelope+"'", "true", "", "")
	statuses := registry.Plugins()
	if len(statuses) != 1 {
		t.Fatalf("plugins = %+v", statuses)
	}
	if statuses[0].BrowserKind != BrowserKindExec {
		t.Fatalf("browser kind = %q, want %q", statuses[0].BrowserKind, BrowserKindExec)
	}
}

// A provider that hangs must not hang the daemon's startup behind it.
func TestBrowserProviderDeadlineIsEnforced(t *testing.T) {
	scripts := t.TempDir()
	root := t.TempDir()
	mint := writeScript(t, filepath.Join(scripts, "mint"), "sleep 30")
	manifest := browserManifest(t, mint, mint)
	browser, _ := manifest["browser"].(map[string]any)
	browser["timeout_ms"] = 250
	writeManifest(t, root, "browser.json", manifest)
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, _, err = registry.OpenBrowserSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hung provider = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("the deadline took %s to fire", elapsed)
	}
}
