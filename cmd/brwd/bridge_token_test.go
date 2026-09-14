package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/extensionbridge"
)

// TestBridgeRequireTokenDefaults pins the posture an unconfigured install runs
// with. Before this, the token was optional unless an operator set an env var
// nothing in setup sets, so every default install accepted a tokenless hello —
// and the Origin check that gets a caller that far is forgeable by any local
// process.
func TestBridgeRequireTokenDefaults(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{"unset requires the token", "", true},
		{"explicit opt-out allows tokenless", "1", false},
		{"true opts out", "true", false},
		{"unrecognised value still requires", "maybe", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRW_BRIDGE_ALLOW_TOKENLESS", tc.env)
			if got := bridgeRequireToken(); got != tc.want {
				t.Fatalf("bridgeRequireToken() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBridgeConstructorRequiresToken proves the safe default lives in the
// library, so a caller that never calls SetRequireToken is still strict.
func TestBridgeConstructorRequiresToken(t *testing.T) {
	b := extensionbridge.New("", 0, "")
	if !b.RequireToken() {
		t.Fatal("a freshly constructed bridge must require a handshake token")
	}
}

// TestBridgeTokenIsNotWrittenByDefault: the token used to be persisted to
// ~/.brw/bridge-token on every launch, where any process running as this user
// could read it without the daemon's cooperation and for as long as the file
// survived — which is forever, because nothing removed it. Nothing in the tree
// ever read it back.
func TestBridgeTokenIsNotWrittenByDefault(t *testing.T) {
	const fixtureToken = "fixture-bridge-token-value-one"

	tests := []struct {
		name      string
		optIn     bool
		preExists bool
		wantFile  string
	}{
		{name: "default writes nothing", wantFile: ""},
		{name: "default removes a file an older daemon left", preExists: true, wantFile: ""},
		{name: "an explicit opt-in still writes", optIn: true, wantFile: fixtureToken},
		{name: "an explicit opt-in overwrites a stale file", optIn: true, preExists: true, wantFile: fixtureToken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".brw", "bridge-token")
			if tc.optIn {
				t.Setenv("BRW_BRIDGE_TOKEN_FILE", path)
			} else {
				t.Setenv("BRW_BRIDGE_TOKEN_FILE", "")
			}
			if tc.preExists {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture-bridge-token-value-two"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			target := bridgeTokenFile("")
			if target.Path != path {
				t.Fatalf("token path = %q, want %q", target.Path, path)
			}
			if err := persistBridgeToken(target, fixtureToken); err != nil {
				t.Fatalf("persistBridgeToken: %v", err)
			}

			data, err := os.ReadFile(path)
			switch {
			case tc.wantFile == "" && err == nil:
				t.Fatalf("%s still holds the handshake token: %q", path, data)
			case tc.wantFile == "" && !os.IsNotExist(err):
				t.Fatalf("reading %s: %v", path, err)
			case tc.wantFile != "" && err != nil:
				t.Fatalf("opted-in token file: %v", err)
			case tc.wantFile != "" && string(data) != tc.wantFile:
				t.Fatalf("token file = %q, want %q", data, tc.wantFile)
			}
		})
	}
}

// TestBridgeTokenFileIsPerWorkspace keeps two bridge daemons on one machine from
// clobbering each other's opted-in file, last writer wins.
func TestBridgeTokenFileIsPerWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRW_BRIDGE_TOKEN_FILE", "")
	first := bridgeTokenFile("work/space")
	second := bridgeTokenFile("other")
	if first.Path == second.Path {
		t.Fatalf("two workspaces share one token file: %s", first.Path)
	}
	if strings.ContainsAny(filepath.Base(first.Path), `/\:`) {
		t.Fatalf("workspace separators leaked into the file name: %s", first.Path)
	}
}
