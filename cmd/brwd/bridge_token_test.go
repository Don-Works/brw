package main

import (
	"os"
	"path/filepath"
	"sort"
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

// TestBridgeTokenSweepClearsEveryCopyAnOlderDaemonLeft: the cleanup used to be
// os.Remove on the single path THIS launch resolved to, which left two files at
// rest that nothing would ever collect. A machine that once ran a
// default-workspace brwd and now runs only workspace-bound ones kept
// ~/.brw/bridge-token forever, and BRW_BRIDGE_TOKEN_FILE pointing anywhere else
// meant the default path was never visited at all. docs/auth-model.md says a
// launch cleans up after the versions that wrote it, so the sweep is the
// directory, not one name.
func TestBridgeTokenSweepClearsEveryCopyAnOlderDaemonLeft(t *testing.T) {
	const fixtureToken = "fixture-bridge-token-value-three"
	const stale = "fixture-bridge-token-value-four"

	tests := []struct {
		name      string
		workspace string
		// optIn is BRW_BRIDGE_TOKEN_FILE: "" unset, "~/<name>" inside ~/.brw,
		// anything else a path outside it.
		optIn    string
		existing []string
		wantKept []string
	}{
		{
			name:      "a workspace-bound daemon still clears the default path",
			workspace: "work",
			existing:  []string{"bridge-token", "bridge-token-work"},
		},
		{
			name:     "the default daemon clears every workspace copy",
			existing: []string{"bridge-token", "bridge-token-work", "bridge-token-other"},
		},
		{
			name:     "an opt-in somewhere else does not excuse the default path",
			optIn:    "elsewhere",
			existing: []string{"bridge-token", "bridge-token-work"},
		},
		{
			name:      "an opt-in inside the directory keeps only that file",
			workspace: "work",
			optIn:     "~/bridge-token-work",
			existing:  []string{"bridge-token", "bridge-token-work", "bridge-token-other"},
			wantKept:  []string{"bridge-token-work"},
		},
		{
			name:     "a file the daemon never wrote is left alone",
			existing: []string{"bridge-token", "notes.txt", "bridge-tokens-backup"},
			wantKept: []string{"notes.txt", "bridge-tokens-backup"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".brw")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.existing {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stale), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			optIn := ""
			switch {
			case strings.HasPrefix(tc.optIn, "~/"):
				optIn = filepath.Join(dir, strings.TrimPrefix(tc.optIn, "~/"))
			case tc.optIn != "":
				optIn = filepath.Join(home, tc.optIn, "bridge-token")
			}
			t.Setenv("BRW_BRIDGE_TOKEN_FILE", optIn)

			if err := persistBridgeToken(bridgeTokenFile(tc.workspace), fixtureToken); err != nil {
				t.Fatalf("persistBridgeToken: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, entry := range entries {
				left = append(left, entry.Name())
			}
			sort.Strings(left)
			want := append([]string(nil), tc.wantKept...)
			sort.Strings(want)
			if strings.Join(left, ",") != strings.Join(want, ",") {
				t.Fatalf("~/.brw holds %v, want %v", left, want)
			}

			if optIn == "" {
				return
			}
			data, err := os.ReadFile(optIn)
			if err != nil {
				t.Fatalf("the opted-in token file: %v", err)
			}
			if string(data) != fixtureToken {
				t.Fatalf("opted-in token file = %q, want %q", data, fixtureToken)
			}
		})
	}
}
