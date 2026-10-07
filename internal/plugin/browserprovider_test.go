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

const fixtureProviderKey = "fixture-provider-key-two"

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

func TestSessionEnvelopeRefusesEverythingButAWellFormedAnswer(t *testing.T) {
	const ok = `{"websocket_url":"ws://127.0.0.1:9222/devtools/browser/abc","session_id":"s-1","expires_in_ms":600000}`
	for name, test := range map[string]struct {
		raw     string
		wantErr string
	}{
		"valid":             {ok, ""},
		"empty":             {``, "valid session envelope"},
		"not json":          {`nope`, "valid session envelope"},
		"unknown field":     {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":1000,"extra":1}`, "valid session envelope"},
		"trailing json":     {ok + `{"x":1}`, "trailing output"},
		"http scheme":       {`{"websocket_url":"http://127.0.0.1:9222/json","session_id":"s","expires_in_ms":1000}`, "must be ws or wss"},
		"file scheme":       {`{"websocket_url":"file:///etc/passwd","session_id":"s","expires_in_ms":1000}`, "must be ws or wss"},
		"no host":           {`{"websocket_url":"ws:///devtools","session_id":"s","expires_in_ms":1000}`, "no host"},
		"userinfo":          {`{"websocket_url":"wss://key:tok@h/p","session_id":"s","expires_in_ms":1000}`, "userinfo"},
		"empty url":         {`{"websocket_url":"","session_id":"s","expires_in_ms":1000}`, "empty websocket URL"},
		"bad session id":    {`{"websocket_url":"ws://h/p","session_id":"s;rm -rf /","expires_in_ms":1000}`, "session_id"},
		"no session id":     {`{"websocket_url":"ws://h/p","session_id":"","expires_in_ms":1000}`, "session_id"},
		"no lifetime":       {`{"websocket_url":"ws://h/p","session_id":"s"}`, "must state expires_in_ms"},
		"negative":          {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":-5}`, "must state expires_in_ms"},
		"lifetime too big":  {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":86400001}`, "lifetime over"},
		"lifetime overflow": {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":18446744083709}`, "lifetime over"},
		"lifetime tiny":     {`{"websocket_url":"ws://h/p","session_id":"s","expires_in_ms":5}`, "lifetime under"},
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

func TestEndpointRedactsOnEveryRenderingPath(t *testing.T) {

	const secretPath = "devtools-session-fixture"
	const endpointURL = "wss://browsers.example/devtools-session-fixture?token=fixture-provider-key-two"
	endpoint, err := ParseEndpoint(endpointURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpointURL, secretPath) || !strings.Contains(endpointURL, fixtureProviderKey) {
		t.Fatal("the fixture URL must carry both secrets, or this test proves nothing")
	}

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

	if !strings.Contains(endpoint.Reveal(), secretPath) {
		t.Fatal("Reveal must return the dialable URL")
	}
}

func providerRegistry(t *testing.T, mintBody, teardownBody, reference, value string) (*Registry, string) {
	t.Helper()
	scripts := t.TempDir()
	root := t.TempDir()

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

	handed, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("teardown recorded nothing: %v", err)
	}
	if string(handed) != "sess-42" {
		t.Fatalf("teardown was handed %q, want the opened session id", handed)
	}
}

func TestProviderCredentialTravelsOnStdinAndNeverInTheArgv(t *testing.T) {

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

	if rendered := fmt.Sprintf("%v", session.Endpoint); strings.Contains(rendered, fixtureProviderKey) {
		t.Fatalf("endpoint rendered as %q, exposing the provider credential", rendered)
	}

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

func TestAPlaintextEndpointToAnotherHostIsDistinguishable(t *testing.T) {
	for name, test := range map[string]struct {
		raw  string
		want bool
	}{
		"plaintext to a host":   {"ws://browsers.example/devtools/browser/fixture", true},
		"plaintext to an ip":    {"ws://198.51.100.7:9222/devtools/browser/fixture", true},
		"encrypted to a host":   {"wss://browsers.example/devtools/browser/fixture", false},
		"loopback v4":           {"ws://127.0.0.1:9222/devtools/browser/fixture", false},
		"loopback v4 elsewhere": {"ws://127.0.0.2:9222/devtools/browser/fixture", false},
		"loopback v6":           {"ws://[::1]:9222/devtools/browser/fixture", false},
		"loopback by name":      {"ws://localhost:9222/devtools/browser/fixture", false},
		"encrypted to loopback": {"wss://127.0.0.1:9222/devtools/browser/fixture", false},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint, err := ParseEndpoint(test.raw)
			if err != nil {
				t.Fatalf("ParseEndpoint(%q) = %v", test.raw, err)
			}
			if got := endpoint.PlaintextToAnotherHost(); got != test.want {
				t.Fatalf("PlaintextToAnotherHost() = %v, want %v", got, test.want)
			}
		})
	}

	if (Endpoint{}).PlaintextToAnotherHost() {
		t.Error("the zero endpoint reported a plaintext socket")
	}
}
