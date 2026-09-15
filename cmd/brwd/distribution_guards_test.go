package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// --remote auto resolves to one of two things, and only one of them is a
// browser the operator named. The startup gate that bars direct CDP is skipped
// for any non-empty --remote, and "auto" is non-empty, so the port-probe half
// of discovery was a way past a profile policy: attach to whatever answers on
// 9222 and then report the policy's own user data directory as the identity of
// what you found.
func TestAutoConnectRefusesAGuessedPortUnderAPolicyThatBarsDirectCDP(t *testing.T) {
	probe := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:9222", Port: 9222, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourcePortProbe}
	fromFile := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:54321", Port: 54321, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourceActivePortFile, From: "/profiles/work"}

	tests := []struct {
		name             string
		endpoint         cdplaunch.AutoEndpoint
		directCDPAllowed bool
		unsafeOverride   bool
		wantRefused      bool
	}{
		{name: "a guessed port under a bridge-only profile", endpoint: probe, wantRefused: true},
		{name: "a guessed port under a direct-CDP profile", endpoint: probe, directCDPAllowed: true},
		{name: "a guessed port with the diagnostic override", endpoint: probe, unsafeOverride: true},
		// The browser wrote this port into the user data directory the policy
		// named, so it is the profile's own browser rather than a guess.
		{name: "the profile's own DevToolsActivePort", endpoint: fromFile, wantRefused: false},
		{name: "no policy at all", endpoint: probe, directCDPAllowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := autoConnectRefusal(tt.endpoint, "chrome-work", tt.directCDPAllowed, tt.unsafeOverride)
			if tt.wantRefused != (err != nil) {
				t.Fatalf("autoConnectRefusal = %v, want refused = %v", err, tt.wantRefused)
			}
			if err == nil {
				return
			}
			// The fix is always to type the endpoint, so the refusal has to name
			// the port it found and the profile that barred it.
			for _, want := range []string{"9222", "chrome-work", tt.endpoint.URL} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// An --upstream-http proxy and the daemon behind it are two URLs and one
// browser. The run lock is keyed on the four profile fields, so a proxy that
// reported none of them took the shared "unidentified" lock while the daemon
// behind it took the profile's own, and the two interleaved on one tab.
func TestProxyAdoptsTheProfileOfTheDaemonItForwardsTo(t *testing.T) {
	upstream := brwidentity.Identity{
		Workspace:        "work",
		Profile:          "chrome-work",
		UserDataDir:      "/profiles/work",
		ProfileDirectory: "Profile 1",
		Mode:             "bridge",
		Transport:        brwidentity.TransportExtensionBridge,
		Headless:         true,
	}
	proxy := brwidentity.Identity{Mode: "upstream-http", Transport: brwidentity.TransportDirectCDP}

	adopted := adoptUpstreamIdentity(proxy, upstream, false)
	if adopted.Workspace != upstream.Workspace || adopted.Profile != upstream.Profile ||
		adopted.UserDataDir != upstream.UserDataDir || adopted.ProfileDirectory != upstream.ProfileDirectory {
		t.Fatalf("a proxy without a policy did not adopt the profile it drives: %+v", adopted)
	}
	// Its own Mode is what it is: "upstream-http" says how the agent reaches
	// brw, and overwriting it would make the proxy claim to be the bridge.
	if adopted.Mode != "upstream-http" {
		t.Errorf("the proxy adopted the upstream's mode: %q", adopted.Mode)
	}
	if adopted.Transport != upstream.Transport || !adopted.Headless {
		t.Errorf("the proxy did not adopt the upstream's transport or headlessness: %+v", adopted)
	}

	// A proxy that has its own policy already verified the upstream against it
	// at startup; adopting would only overwrite equals, and a bug in that
	// verification must not be papered over here.
	pinned := brwidentity.Identity{Workspace: "other", Profile: "chrome-other", Mode: "upstream-http"}
	if got := adoptUpstreamIdentity(pinned, upstream, true); got.Workspace != "other" || got.Profile != "chrome-other" {
		t.Errorf("a proxy with its own profile policy took the upstream's profile: %+v", got)
	}

	// An upstream that says nothing leaves the proxy as it was rather than
	// blanking the transport it already knew.
	if got := adoptUpstreamIdentity(proxy, brwidentity.Identity{}, false); got != proxy {
		t.Errorf("an empty upstream identity changed the proxy's: %+v", got)
	}
}

// --idle-exit measures the HTTP API. With no listener it can never fire, and a
// flag that silently does nothing is worse on a daemon whose whole job was to
// stop itself.
func TestIdleExitRefusesADaemonWithNoHTTPListener(t *testing.T) {
	tests := []struct {
		name        string
		idleExit    time.Duration
		httpAddr    string
		wantRefused bool
	}{
		{name: "armed with the listener on", idleExit: 30 * time.Minute, httpAddr: "127.0.0.1:17310"},
		{name: "armed with the listener off", idleExit: 30 * time.Minute, httpAddr: "off", wantRefused: true},
		{name: "armed with an empty listen address", idleExit: 30 * time.Minute, httpAddr: "", wantRefused: true},
		{name: "not armed, listener off", idleExit: 0, httpAddr: "off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := idleExitRefusal(tt.idleExit, tt.httpAddr)
			if tt.wantRefused != (err != nil) {
				t.Fatalf("idleExitRefusal = %v, want refused = %v", err, tt.wantRefused)
			}
			if err != nil && !strings.Contains(err.Error(), "--mcp-idle-exit") {
				t.Errorf("the refusal does not name the flag that does work here: %v", err)
			}
		})
	}
}

// Each of the three guards above is a decision main() has to take, and a
// function nothing calls decides nothing. Read out of the source because a
// second hand-written list is exactly what drifts.
func TestTheDistributionGuardsAreWiredIntoStartup(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	for _, call := range []string{
		"idleExitRefusal(httpIdleExit, httpAddr)",
		"autoConnectRefusal(endpoint,",
		"adoptUpstreamIdentity(runtimeIdentity, health.Identity, haveProfilePolicy)",
		"adoptUpstreamIdentity(usageIdentity, health.Identity, haveProfilePolicy)",
		"server.SetActivityHook(api.NoteActivity)",
	} {
		if !strings.Contains(text, call) {
			t.Errorf("main.go no longer calls %s, so the guard it implements never runs", call)
		}
	}
}
