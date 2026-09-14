package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshExtensionPayloadsKeepsPerInstallState: the refresh replaces
// executable code and nothing else. Each copy's bridge endpoint and handshake
// token is its own: losing it leaves the browser presenting a token the daemon
// no longer knows, and inheriting another profile's makes the extension connect
// to the wrong profile's daemon.
func TestRefreshExtensionPayloadsKeepsPerInstallState(t *testing.T) {
	const ownDefaults = `{"endpoint":"ws://127.0.0.1:1/x","token":"per-profile"}`
	// The refresh source is the installed extension, which on a configured
	// machine carries the default profile's own endpoint and token.
	const sourceDefaults = `{"endpoint":"ws://127.0.0.1:2/x","token":"default-profile"}`

	cases := []struct {
		name           string
		sourceDefaults string
		copyDefaults   string
		wantDefaults   string
	}{
		{
			name:           "a copy's own bridge defaults survive",
			sourceDefaults: sourceDefaults,
			copyDefaults:   ownDefaults,
			wantDefaults:   ownDefaults,
		},
		{
			name:           "a copy with none does not inherit the source's",
			sourceDefaults: sourceDefaults,
			wantDefaults:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appDir := t.TempDir()
			writeFixture(t, filepath.Join(appDir, "extension", "manifest.json"), `{"version":"2.0.0"}`)
			writeFixture(t, filepath.Join(appDir, "extension", "background.js"), "// 2.0.0\n")
			if tc.sourceDefaults != "" {
				writeFixture(t, filepath.Join(appDir, "extension", BridgeDefaultsFile), tc.sourceDefaults)
			}

			writeFixture(t, filepath.Join(appDir, "extension-work", "manifest.json"), `{"version":"1.0.0"}`)
			writeFixture(t, filepath.Join(appDir, "extension-work", "background.js"), "// 1.0.0\n")
			writeFixture(t, filepath.Join(appDir, "extension-work", "removed.js"), "// gone in 2.0.0\n")
			if tc.copyDefaults != "" {
				writeFixture(t, filepath.Join(appDir, "extension-work", BridgeDefaultsFile), tc.copyDefaults)
			}

			refreshed, err := RefreshExtensionPayloads(appDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(refreshed) != 1 || refreshed[0] != "extension-work" {
				t.Fatalf("refreshed = %v", refreshed)
			}
			version, err := ExtensionPayloadVersion(filepath.Join(appDir, "extension-work"))
			if err != nil || version != "2.0.0" {
				t.Fatalf("per-profile payload = %q (%v)", version, err)
			}
			if _, err := os.Stat(filepath.Join(appDir, "extension-work", "removed.js")); !os.IsNotExist(err) {
				t.Fatalf("a file the new payload does not have survived as loadable code: %v", err)
			}

			path := filepath.Join(appDir, "extension-work", BridgeDefaultsFile)
			data, err := os.ReadFile(path)
			switch {
			case tc.wantDefaults == "":
				if err == nil {
					t.Fatalf("the copy inherited bridge defaults it never had: %q", data)
				}
				if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			case err != nil || string(data) != tc.wantDefaults:
				t.Fatalf("bridge defaults = %q (%v), want %q", data, err, tc.wantDefaults)
			}
		})
	}
}

// TestRefreshExtensionPayloadsLeavesSymlinkedCopiesAlone: a symlinked payload is
// a developer pointing a profile at a checkout, and replacing it would detach
// them from the tree they are editing.
func TestRefreshExtensionPayloadsLeavesSymlinkedCopiesAlone(t *testing.T) {
	appDir := t.TempDir()
	writeFixture(t, filepath.Join(appDir, "extension", "manifest.json"), `{"version":"2.0.0"}`)
	checkout := filepath.Join(appDir, "checkout")
	writeFixture(t, filepath.Join(checkout, "manifest.json"), `{"version":"0.0.1"}`)
	if err := os.Symlink(checkout, filepath.Join(appDir, "extension-dev")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	refreshed, err := RefreshExtensionPayloads(appDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 0 {
		t.Fatalf("refreshed = %v, want the symlinked copy left alone", refreshed)
	}
	version, err := ExtensionPayloadVersion(filepath.Join(checkout))
	if err != nil || version != "0.0.1" {
		t.Fatalf("the symlinked checkout was overwritten: %q (%v)", version, err)
	}
}

// TestInstallPayloadReplacesOnlyWhatTheArchiveOwns: config/ and a per-profile
// copy's own state are outside the payload, so an install cannot reach them.
func TestInstallPayloadReplacesOnlyWhatTheArchiveOwns(t *testing.T) {
	appDir := t.TempDir()
	unpacked := t.TempDir()

	writeFixture(t, filepath.Join(unpacked, "bin", "brwd"), "new brwd")
	if err := os.Chmod(filepath.Join(unpacked, "bin", "brwd"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(unpacked, "extension", "manifest.json"), `{"version":"2.0.0"}`)

	writeFixture(t, filepath.Join(appDir, "bin", "brwd"), "old brwd")
	writeFixture(t, filepath.Join(appDir, "bin", "stale-helper"), "removed in 2.0.0")
	writeFixture(t, filepath.Join(appDir, "extension", "manifest.json"), `{"version":"1.0.0"}`)
	writeFixture(t, filepath.Join(appDir, "extension", BridgeDefaultsFile), `{"endpoint":"ws://127.0.0.1:1/x"}`)
	writeFixture(t, filepath.Join(appDir, "config", "browser-profiles.json"), "{}")
	writeFixture(t, filepath.Join(appDir, "extension-work", "manifest.json"), `{"version":"1.0.0"}`)

	refreshed, err := InstallPayload(unpacked, appDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 1 || refreshed[0] != "extension-work" {
		t.Fatalf("refreshed = %v", refreshed)
	}
	data, err := os.ReadFile(filepath.Join(appDir, "bin", "brwd"))
	if err != nil || string(data) != "new brwd" {
		t.Fatalf("brwd = %q (%v)", data, err)
	}
	info, err := os.Stat(filepath.Join(appDir, "bin", "brwd"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("brwd lost its executable bit: %v (%v)", info.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "bin", "stale-helper")); !os.IsNotExist(err) {
		t.Fatalf("a command the release dropped is still installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "config", "browser-profiles.json")); err != nil {
		t.Fatalf("the install reached into config/: %v", err)
	}
	defaults, err := os.ReadFile(filepath.Join(appDir, "extension", BridgeDefaultsFile))
	if err != nil || string(defaults) != `{"endpoint":"ws://127.0.0.1:1/x"}` {
		t.Fatalf("installed extension bridge defaults = %q (%v)", defaults, err)
	}
	// The refresh that follows the install copies from appDir/extension, which
	// now holds the default profile's endpoint and token again. A profile copy
	// that has none of its own must not come out of the install holding them.
	if data, err := os.ReadFile(filepath.Join(appDir, "extension-work", BridgeDefaultsFile)); err == nil {
		t.Fatalf("extension-work inherited another profile's bridge defaults: %q", data)
	}
	for _, dir := range []string{"extension", "extension-work"} {
		version, err := ExtensionPayloadVersion(filepath.Join(appDir, dir))
		if err != nil || version != "2.0.0" {
			t.Fatalf("%s = %q (%v)", dir, version, err)
		}
	}
}
