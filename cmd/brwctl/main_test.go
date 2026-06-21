package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBridgeExtensionID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestChromeExtensionInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Preferences"), []byte(`{
		"extensions": {
			"settings": {
				"`+testBridgeExtensionID+`": {"state": 1}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, source, err := chromeExtensionInstalled(dir, testBridgeExtensionID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected extension to be detected")
	}
	if source == "" {
		t.Fatal("expected source path")
	}
}

func TestQuoteRemoteHomePathWithSpaces(t *testing.T) {
	got := quoteRemote("~/Library/Application Support/brw/bin/brwd")
	want := `"$HOME/Library/Application Support/brw/bin/brwd"`
	if got != want {
		t.Fatalf("quoteRemote = %q, want %q", got, want)
	}
}

func TestRemoteMCPWrapperScript(t *testing.T) {
	script := remoteMCPWrapperScript(remoteMCPWrapperOptions{
		Host:                  "max-air",
		User:                  "maxrevitt",
		RemoteBRWD:            "~/.local/bin/brwd",
		RemoteHTTP:            "http://127.0.0.1:17310",
		MCPTools:              "core",
		SSH:                   "/usr/bin/ssh",
		ConnectTimeout:        "7",
		KnownHosts:            "/tmp/brw known_hosts",
		StrictHostKeyChecking: "accept-new",
		LogPath:               "/tmp/brw remote.log",
		SSHOptions:            []string{"ProxyJump=bastion"},
	})
	for _, want := range []string{
		"BRW_REMOTE=${BRW_REMOTE:-'maxrevitt@max-air'}",
		"BRW_KNOWN_HOSTS=${BRW_KNOWN_HOSTS:-'/tmp/brw known_hosts'}",
		"-o UserKnownHostsFile=\"$BRW_KNOWN_HOSTS\"",
		"-o StrictHostKeyChecking=\"$BRW_STRICT_HOST_KEY_CHECKING\"",
		"-o 'ProxyJump=bastion'",
		`"$HOME/.local/bin/brwd"`,
		`http://127.0.0.1:17310`,
		`--mcp-tools`,
		`core`,
		`2>>"$BRW_LOG"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper script missing %q\n%s", want, script)
		}
	}
}

func TestRemoteMCPWrapperWritesExecutableOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "brw-remote")
	if err := remoteMCPWrapper([]string{
		"--host", "max-air",
		"--remote-brwd", "~/.local/bin/brwd",
		"--known-hosts", filepath.Join(t.TempDir(), "known_hosts"),
		"--log", filepath.Join(t.TempDir(), "remote.log"),
		"--output", out,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & 0o777; got != 0o755 {
		t.Fatalf("mode = %v, want 0755", got)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "BRW_REMOTE=${BRW_REMOTE:-'max-air'}") {
		t.Fatalf("generated wrapper missing host:\n%s", data)
	}
}
