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
// executable code and nothing else. Losing a copy's bridge-defaults.json would
// leave the browser presenting a token the daemon no longer knows.
func TestRefreshExtensionPayloadsKeepsPerInstallState(t *testing.T) {
	appDir := t.TempDir()
	writeFixture(t, filepath.Join(appDir, "extension", "manifest.json"), `{"version":"2.0.0"}`)
	writeFixture(t, filepath.Join(appDir, "extension", "background.js"), "// 2.0.0\n")

	writeFixture(t, filepath.Join(appDir, "extension-work", "manifest.json"), `{"version":"1.0.0"}`)
	writeFixture(t, filepath.Join(appDir, "extension-work", "background.js"), "// 1.0.0\n")
	writeFixture(t, filepath.Join(appDir, "extension-work", "removed.js"), "// gone in 2.0.0\n")
	writeFixture(t, filepath.Join(appDir, "extension-work", BridgeDefaultsFile), `{"endpoint":"ws://127.0.0.1:1/x"}`)

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
	data, err := os.ReadFile(filepath.Join(appDir, "extension-work", BridgeDefaultsFile))
	if err != nil || string(data) != `{"endpoint":"ws://127.0.0.1:1/x"}` {
		t.Fatalf("bridge defaults = %q (%v)", data, err)
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
	for _, dir := range []string{"extension", "extension-work"} {
		version, err := ExtensionPayloadVersion(filepath.Join(appDir, dir))
		if err != nil || version != "2.0.0" {
			t.Fatalf("%s = %q (%v)", dir, version, err)
		}
	}
}
