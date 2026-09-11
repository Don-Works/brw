package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

// TestDetectClaudeInChrome fixes the rule that keeps this warning from becoming
// noise: it fires only on a positive signal, and an explicit false wins over
// the older onboarding heuristics.
func TestDetectClaudeInChrome(t *testing.T) {
	cases := []struct {
		name        string
		write       bool
		content     string
		wantEnabled bool
		wantSignal  string
	}{
		{
			name:        "no claude config at all",
			wantEnabled: false,
		},
		{
			name:        "malformed json is tolerated",
			write:       true,
			content:     `{"claudeInChromeDefaultEnabled": tru`,
			wantEnabled: false,
		},
		{
			name:        "empty object",
			write:       true,
			content:     `{}`,
			wantEnabled: false,
		},
		{
			name:        "integration explicitly on",
			write:       true,
			content:     `{"claudeInChromeDefaultEnabled": true, "numStartups": 12}`,
			wantEnabled: true,
			wantSignal:  "claudeInChromeDefaultEnabled",
		},
		{
			name:        "integration explicitly off wins over the onboarding pair",
			write:       true,
			content:     `{"claudeInChromeDefaultEnabled": false, "hasCompletedClaudeInChromeOnboarding": true, "cachedChromeExtensionInstalled": true}`,
			wantEnabled: false,
		},
		{
			name:        "older config with only the onboarding pair",
			write:       true,
			content:     `{"hasCompletedClaudeInChromeOnboarding": true, "cachedChromeExtensionInstalled": true}`,
			wantEnabled: true,
			wantSignal:  "hasCompletedClaudeInChromeOnboarding",
		},
		{
			name:        "onboarded but the extension is gone",
			write:       true,
			content:     `{"hasCompletedClaudeInChromeOnboarding": true, "cachedChromeExtensionInstalled": false}`,
			wantEnabled: false,
		},
		{
			name:        "unrelated keys only",
			write:       true,
			content:     `{"tipsHistory": {}, "installMethod": "native"}`,
			wantEnabled: false,
		},
		{
			name:        "a non-boolean value is not a signal",
			write:       true,
			content:     `{"claudeInChromeDefaultEnabled": "yes"}`,
			wantEnabled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".claude.json")
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			state := DetectClaudeInChrome(path)
			if state.Enabled != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", state.Enabled, tc.wantEnabled)
			}
			if !tc.wantEnabled {
				if len(state.Signals) != 0 {
					t.Fatalf("a silent result must carry no signals, got %v", state.Signals)
				}
				return
			}
			if len(state.Signals) == 0 || state.Signals[0] != tc.wantSignal {
				t.Fatalf("signals = %v, want %q first", state.Signals, tc.wantSignal)
			}
			if state.Path != path {
				t.Fatalf("path = %q, want %q", state.Path, path)
			}
		})
	}
}

func TestClaudeConfigPath(t *testing.T) {
	if got := ClaudeConfigPath("/home/someone"); got != "/home/someone/.claude.json" {
		t.Fatalf("ClaudeConfigPath = %q", got)
	}
}

func TestCapabilitiesNameBothLanes(t *testing.T) {
	bridge := CapabilitiesFor(ResolvedExtensionBridge)
	if bridge.Transport != ResolvedExtensionBridge {
		t.Fatalf("transport = %q", bridge.Transport)
	}
	for _, want := range []string{"brw_open_incognito", "brw_cookies"} {
		if !strings.Contains(bridge.Lacks, want) {
			t.Fatalf("bridge capabilities must name %q as missing: %q", want, bridge.Lacks)
		}
	}
	direct := CapabilitiesFor(ResolvedDirectCDP)
	for _, want := range []string{"brw_open_incognito", "brw_cookies"} {
		if !strings.Contains(direct.Has, want) {
			t.Fatalf("direct-cdp capabilities must name %q as present: %q", want, direct.Has)
		}
	}
	if !strings.Contains(direct.Lacks, "separate") {
		t.Fatalf("direct-cdp must say it drives a separate browser: %q", direct.Lacks)
	}
}

func TestResolvedTransport(t *testing.T) {
	cases := []struct {
		name    string
		profile profilepolicy.Profile
		want    string
	}{
		{"bridge only", profilepolicy.Profile{ExtensionBridgeAllowed: true}, ResolvedExtensionBridge},
		{"direct only", profilepolicy.Profile{DirectCDPAllowed: true}, ResolvedDirectCDP},
		// Matches `mcp-config --mode auto`, which prefers direct when a profile
		// allows both; doctor must name the lane the daemon will actually use.
		{"both allowed prefers direct", profilepolicy.Profile{DirectCDPAllowed: true, ExtensionBridgeAllowed: true}, ResolvedDirectCDP},
		{"neither", profilepolicy.Profile{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvedTransport(tc.profile); got != tc.want {
				t.Fatalf("ResolvedTransport = %q, want %q", got, tc.want)
			}
		})
	}
}
