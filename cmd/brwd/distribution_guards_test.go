package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

const policyDir = "/profiles/work"

func TestAutoConnectRefusalGatesOnTheBrowserNotTheDiscoveryLane(t *testing.T) {
	probe := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:9222", Port: 9222, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourcePortProbe}
	ownBrowser := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:54321", Port: 54321, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourceActivePortFile, From: policyDir}
	otherBrowser := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:54322", Port: 54322, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourceActivePortFile, From: "/var/tmp/another-chrome-profile"}
	spelledDifferently := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:54323", Port: 54323, Browser: "Chrome/141.0.0.0", Source: cdplaunch.SourceActivePortFile, From: policyDir + "/./"}

	tests := []struct {
		name              string
		endpoint          cdplaunch.AutoEndpoint
		policyUserDataDir string
		directCDPAllowed  bool
		unsafeOverride    bool
		wantRefused       bool
	}{
		{name: "a guessed port under a bridge-only profile", endpoint: probe, policyUserDataDir: policyDir, wantRefused: true},

		{name: "another browser's DevToolsActivePort", endpoint: otherBrowser, policyUserDataDir: policyDir, wantRefused: true},
		{name: "the policy's own browser", endpoint: ownBrowser, policyUserDataDir: policyDir},
		{name: "the policy's own browser, spelled differently", endpoint: spelledDifferently, policyUserDataDir: policyDir},

		{name: "a bridge-only profile that names no directory", endpoint: ownBrowser, wantRefused: true},
		{name: "a guessed port under a direct-CDP profile", endpoint: probe, policyUserDataDir: policyDir, directCDPAllowed: true},
		{name: "another browser under a direct-CDP profile", endpoint: otherBrowser, policyUserDataDir: policyDir, directCDPAllowed: true},
		{name: "another browser with the diagnostic override", endpoint: otherBrowser, policyUserDataDir: policyDir, unsafeOverride: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := autoConnectRefusal(tt.endpoint, "chrome-work", tt.policyUserDataDir, tt.directCDPAllowed, tt.unsafeOverride)
			if tt.wantRefused != (err != nil) {
				t.Fatalf("autoConnectRefusal = %v, want refused = %v", err, tt.wantRefused)
			}
			if err == nil {
				return
			}

			for _, want := range []string{strconv.Itoa(tt.endpoint.Port), "chrome-work", tt.endpoint.URL} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

func TestEveryDiscoverySourceIsGatedOnTheDirectoryItNames(t *testing.T) {
	sources := discoverySources(t)
	if len(sources) < 2 {
		t.Fatalf("found %d discovery sources in the cdp package, so this is not enumerating anything: %v", len(sources), sources)
	}
	for _, source := range sources {
		t.Run(source, func(t *testing.T) {
			endpoint := cdplaunch.AutoEndpoint{URL: "http://127.0.0.1:9222", Port: 9222, Source: source}

			endpoint.From = ""
			if err := autoConnectRefusal(endpoint, "chrome-work", policyDir, false, false); err == nil {
				t.Errorf("an endpoint found by %s that names no user data directory was accepted under a bridge-only policy", source)
			}
			endpoint.From = "/somewhere/else"
			if err := autoConnectRefusal(endpoint, "chrome-work", policyDir, false, false); err == nil {
				t.Errorf("an endpoint found by %s in another browser's directory was accepted under a bridge-only policy", source)
			}
			endpoint.From = policyDir
			if err := autoConnectRefusal(endpoint, "chrome-work", policyDir, false, false); err != nil {
				t.Errorf("the policy's own browser was refused because it was found by %s: %v", source, err)
			}
		})
	}
}

func discoverySources(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "cdp", "autoconnect.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var sources []string
	for _, decl := range parsed.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Source") || index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s is not a string constant, so this test cannot hold it to the property", name.Name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				sources = append(sources, unquoted)
			}
		}
	}
	return sources
}

func TestAutoConnectAlsoSearchesADirectoryTheProfilePolicyDidNotName(t *testing.T) {
	dirs := autoConnectSearchDirs(policyDir)
	if len(dirs) == 0 || dirs[0] != policyDir {
		t.Fatalf("the policy's own directory is not searched first: %v", dirs)
	}
	fallback := cdplaunch.DefaultProfileDir("")
	if fallback == "" {
		t.Skip("this machine has no home directory, so there is no second search directory")
	}
	found := false
	for _, dir := range dirs[1:] {
		if dir == fallback {
			found = true
		}
		if sameUserDataDir(dir, policyDir) {
			t.Fatalf("search directory %q is the policy's own, so this proves nothing", dir)
		}
	}
	if !found {
		t.Fatalf("discovery no longer searches %s, so a DevToolsActivePort hit may now be the policy's own directory: %v", fallback, dirs)
	}
}

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

	if adopted.Mode != "upstream-http" {
		t.Errorf("the proxy adopted the upstream's mode: %q", adopted.Mode)
	}
	if adopted.Transport != upstream.Transport || !adopted.Headless {
		t.Errorf("the proxy did not adopt the upstream's transport or headlessness: %+v", adopted)
	}

	pinned := brwidentity.Identity{Workspace: "other", Profile: "chrome-other", Mode: "upstream-http"}
	if got := adoptUpstreamIdentity(pinned, upstream, true); got.Workspace != "other" || got.Profile != "chrome-other" {
		t.Errorf("a proxy with its own profile policy took the upstream's profile: %+v", got)
	}

	if got := adoptUpstreamIdentity(proxy, brwidentity.Identity{}, false); got != proxy {
		t.Errorf("an empty upstream identity changed the proxy's: %+v", got)
	}
}

func TestResolveIdleExitCarriesTheDurationRatherThanRefusing(t *testing.T) {
	const idle = 30 * time.Minute
	tests := []struct {
		name        string
		idleExit    time.Duration
		mcpIdleExit time.Duration
		httpAddr    string
		mcpMode     bool
		mcpTyped    bool
		wantMCP     time.Duration
		wantNote    string
	}{
		{name: "the listener is on, so the flag works where it was aimed", idleExit: idle, httpAddr: "127.0.0.1:17310"},
		{name: "not armed at all", httpAddr: "off"},
		{
			name:     "no listener on a stdio daemon arms the stdio watcher",
			idleExit: idle, httpAddr: "off", mcpMode: true,
			wantMCP: idle, wantNote: "arming the stdio idle exit",
		},
		{
			name:     "an empty listen address is no listener either",
			idleExit: idle, httpAddr: "", mcpMode: true,
			wantMCP: idle, wantNote: "arming the stdio idle exit",
		},
		{
			name:     "a --mcp-idle-exit the operator typed is the one that wins",
			idleExit: idle, mcpIdleExit: 5 * time.Minute, httpAddr: "off", mcpMode: true, mcpTyped: true,
			wantMCP: 5 * time.Minute, wantNote: "--mcp-idle-exit you set (5m0s)",
		},
		{
			name:     "a typed zero turns the stdio watcher off and stays off",
			idleExit: idle, httpAddr: "off", mcpMode: true, mcpTyped: true,
			wantNote: "--mcp-idle-exit you set (0s)",
		},

		{
			name:     "an inherited proxy default does not outrank a typed --idle-exit",
			idleExit: idle, mcpIdleExit: 90 * time.Minute, httpAddr: "off", mcpMode: true,
			wantMCP: idle, wantNote: "arming the stdio idle exit",
		},
		{
			name:     "no listener and no stdio session measures nothing, and says so",
			idleExit: idle, httpAddr: "off",
			wantNote: "never fire",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMCP, note := resolveIdleExit(tt.idleExit, tt.mcpIdleExit, tt.httpAddr, tt.mcpMode, tt.mcpTyped)
			if gotMCP != tt.wantMCP {
				t.Errorf("--mcp-idle-exit resolved to %s, want %s", gotMCP, tt.wantMCP)
			}
			if tt.wantNote == "" {
				if note != "" {
					t.Errorf("an ordinary invocation logged %q", note)
				}
				return
			}
			if !strings.Contains(note, tt.wantNote) {
				t.Errorf("the log line does not say what happened (%q): %q", tt.wantNote, note)
			}
			if strings.Contains(note, "refus") {
				t.Errorf("the flag combination is still reported as a refusal: %q", note)
			}
		})
	}
}
