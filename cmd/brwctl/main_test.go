package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/recipe"
)

const testBridgeExtensionID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestInstallPrivateRecipeIsImmutableAndMakesRepairVersionSearchable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-recipes")
	value := recipe.Recipe{
		SchemaVersion: recipe.SchemaVersion,
		ID:            "example.reports.capture",
		Version:       "1.9.0",
		Name:          "Capture reusable report",
		Description:   "Open and capture a synthetic report for a local test.",
		Intents:       []string{"capture reusable report", "save report text"},
		Origins:       []string{"https://reports.example.test"},
		Risk:          "read_only",
		Steps: []recipe.Step{
			{ID: "open", Action: "navigate_to", Effect: "read", URL: "https://reports.example.test/report"},
			{ID: "capture", Action: "capture", Capture: &recipe.CaptureSpec{Kind: "text"}},
		},
	}
	first, err := installPrivateRecipe(root, value)
	if err != nil || first.Action != "installed" {
		t.Fatalf("first install=%+v err=%v", first, err)
	}
	deduped, err := installPrivateRecipe(root, value)
	if err != nil || deduped.Action != "deduped" || deduped.Digest != first.Digest {
		t.Fatalf("dedupe=%+v err=%v", deduped, err)
	}

	changedInPlace := value
	changedInPlace.Description = "Silently changed executable version."
	if _, err := installPrivateRecipe(root, changedInPlace); err == nil || !strings.Contains(err.Error(), "new semantic version") {
		t.Fatalf("same-version mutation error=%v", err)
	}

	repaired := value
	repaired.Version = "1.10.0"
	repaired.Description = "Updated semantic report capture after the page changed."
	second, err := installPrivateRecipe(root, repaired)
	if err != nil || second.Action != "installed" {
		t.Fatalf("repair install=%+v err=%v", second, err)
	}
	provider, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := provider.Search(context.Background(), "capture reusable report", "", 10)
	if err != nil || len(matches) != 1 || matches[0].Version != "1.10.0" {
		t.Fatalf("repair search=%+v err=%v", matches, err)
	}
	if _, err := provider.Fetch(context.Background(), value.ID, value.Version, first.Digest); err != nil {
		t.Fatalf("old immutable version no longer fetchable: %v", err)
	}
}

func TestRecipeInstallSourceMustBeOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecipeSource(path, true); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("broad recipe draft permissions error=%v", err)
	}
	if _, err := readRecipeSource(path, false); err != nil {
		t.Fatalf("validation should permit a non-private draft: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecipeSource(path, true); err != nil {
		t.Fatalf("owner-only recipe draft rejected: %v", err)
	}
}

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
		Host:                  "browser-host",
		User:                  "browser-user",
		RemoteBRWD:            "~/.local/bin/brwd",
		RemoteHTTP:            "http://127.0.0.1:17310",
		MCPTools:              "core",
		SSH:                   "/usr/bin/ssh",
		ConnectTimeout:        "7",
		ConnectionAttempts:    "2",
		ServerAliveInterval:   "30",
		ServerAliveCountMax:   "3",
		KnownHosts:            "/tmp/brw known_hosts",
		StrictHostKeyChecking: "accept-new",
		LogPath:               "/tmp/brw remote.log",
		LogMaxBytes:           "5242880",
		SSHOptions:            []string{"ProxyJump=bastion"},
	})
	for _, want := range []string{
		"BRW_REMOTE=${BRW_REMOTE:-'browser-user@browser-host'}",
		"BRW_KNOWN_HOSTS=${BRW_KNOWN_HOSTS:-'/tmp/brw known_hosts'}",
		"-o BatchMode=yes",
		"-o RequestTTY=no",
		"-o UserKnownHostsFile=\"$BRW_KNOWN_HOSTS\"",
		"-o StrictHostKeyChecking=\"$BRW_STRICT_HOST_KEY_CHECKING\"",
		"-o ConnectionAttempts=\"$BRW_CONNECTION_ATTEMPTS\"",
		"-o ServerAliveInterval=\"$BRW_SERVER_ALIVE_INTERVAL\"",
		"-o ServerAliveCountMax=\"$BRW_SERVER_ALIVE_COUNT_MAX\"",
		"BRW_LOG_MAX_BYTES=${BRW_LOG_MAX_BYTES:-'5242880'}",
		`mv -f "$BRW_LOG" "$BRW_LOG.1"`,
		"-o 'ProxyJump=bastion'",
		`"$HOME/.local/bin/brwd"`,
		// Contract lock: the remote daemon must keep its listener off and proxy
		// to the loopback HTTP API. A silent flag rename here would break the wrapper.
		`--upstream-http`,
		`http://127.0.0.1:17310`,
		`--http`,
		`off`,
		`--mcp-tools`,
		`core`,
		`2>>"$BRW_LOG"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper script missing %q\n%s", want, script)
		}
	}
	// Identity pinning and compression are opt-in: absent unless requested.
	for _, unwanted := range []string{"IdentityFile", "IdentitiesOnly", "Compression=yes"} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("wrapper script unexpectedly contains %q\n%s", unwanted, script)
		}
	}
}

func TestRuntimeArgsUseProfileBridgeAddresses(t *testing.T) {
	profile := profilepolicy.Profile{
		BridgeHTTPAddr: "127.0.0.1:17410",
		BridgeWSAddr:   "127.0.0.1:17411",
	}
	bridge := strings.Join(runtimeArgs("bridge", profile), " ")
	for _, want := range []string{"--bridge", "--bridge-addr 127.0.0.1:17411"} {
		if !strings.Contains(bridge, want) {
			t.Fatalf("bridge args missing %q: %s", want, bridge)
		}
	}
	upstream := strings.Join(runtimeArgs("upstream-http", profile), " ")
	if !strings.Contains(upstream, "--upstream-http http://127.0.0.1:17410") {
		t.Fatalf("upstream args did not use policy HTTP addr: %s", upstream)
	}
}

func TestRemoteMCPWrapperIdentityAndCompression(t *testing.T) {
	script := remoteMCPWrapperScript(remoteMCPWrapperOptions{
		Host:                  "browser-host",
		RemoteBRWD:            "brwd",
		RemoteHTTP:            "http://127.0.0.1:17310",
		MCPTools:              "all",
		SSH:                   "ssh",
		ConnectTimeout:        "5",
		ConnectionAttempts:    "1",
		ServerAliveInterval:   "30",
		ServerAliveCountMax:   "3",
		KnownHosts:            "/tmp/known_hosts",
		StrictHostKeyChecking: "yes",
		IdentityFile:          "~/.ssh/id_brw",
		Compression:           true,
		LogPath:               "/tmp/remote.log",
		LogMaxBytes:           "0",
	})
	for _, want := range []string{
		"-o IdentityFile='~/.ssh/id_brw'",
		"-o IdentitiesOnly=yes",
		"-o Compression=yes",
		// Rotation is always written but runtime-gated; 0 baked in disables it.
		"BRW_LOG_MAX_BYTES=${BRW_LOG_MAX_BYTES:-'0'}",
		`if [ "$BRW_LOG_MAX_BYTES" -gt 0 ] && [ -f "$BRW_LOG" ]; then`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper script missing %q\n%s", want, script)
		}
	}
}

func TestRemoteMCPWrapperRepeatedSSHOptions(t *testing.T) {
	script := remoteMCPWrapperScript(remoteMCPWrapperOptions{
		Host: "h", RemoteBRWD: "brwd", RemoteHTTP: "http://127.0.0.1:17310",
		MCPTools: "all", SSH: "ssh", ConnectTimeout: "5", ConnectionAttempts: "1",
		ServerAliveInterval: "30", ServerAliveCountMax: "3", KnownHosts: "/tmp/kh",
		StrictHostKeyChecking: "accept-new", LogPath: "/tmp/l", LogMaxBytes: "1",
		SSHOptions: []string{"ProxyJump=bastion", "Compression=yes"},
	})
	if c := strings.Count(script, "-o 'ProxyJump=bastion'"); c != 1 {
		t.Fatalf("ProxyJump option count = %d, want 1\n%s", c, script)
	}
	if !strings.Contains(script, "-o 'Compression=yes'") {
		t.Fatalf("second --ssh-option missing\n%s", script)
	}
}

func TestRemoteMCPWrapperValidation(t *testing.T) {
	base := func() []string {
		return []string{
			"--host", "browser-host",
			"--remote-brwd", "brwd",
			"--known-hosts", filepath.Join(t.TempDir(), "kh"),
			"--log", filepath.Join(t.TempDir(), "l.log"),
			"--output", filepath.Join(t.TempDir(), "out"),
		}
	}
	cases := []struct {
		name string
		args []string
	}{
		{"missing host", []string{"--remote-brwd", "brwd"}},
		{"user with user@host", append(base(), "--host", "u@browser-host", "--user", "u")},
		{"bad strict-host-key-checking", append(base(), "--strict-host-key-checking", "maybe")},
		{"bad mcp-tools", append(base(), "--mcp-tools", "some")},
		{"negative keepalive", append(base(), "--server-alive-interval", "-1")},
		{"non-numeric log-max-bytes", append(base(), "--log-max-bytes", "lots")},
		{"non-numeric connection-attempts", append(base(), "--connection-attempts", "x")},
		{"zero connection-attempts", append(base(), "--connection-attempts", "0")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := remoteMCPWrapper(tc.args); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestRemoteMCPWrapperWritesExecutableOutput(t *testing.T) {
	t.Setenv("BRW_MCP_TOOLS", "")
	out := filepath.Join(t.TempDir(), "brw-remote")
	if err := remoteMCPWrapper([]string{
		"--host", "browser-host",
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
	if !strings.Contains(string(data), "BRW_REMOTE=${BRW_REMOTE:-'browser-host'}") {
		t.Fatalf("generated wrapper missing host:\n%s", data)
	}
	if !strings.Contains(string(data), "--mcp-tools") || !strings.Contains(string(data), "auto") {
		t.Fatalf("generated wrapper did not default to token-saving auto profile:\n%s", data)
	}
}

func TestRemoteMCPWrapperAcceptsAllAdvertisedToolProfiles(t *testing.T) {
	for _, profile := range []string{"all", "core", "minimal", "auto"} {
		t.Run(profile, func(t *testing.T) {
			root := t.TempDir()
			err := remoteMCPWrapper([]string{
				"--host", "browser-host", "--mcp-tools", profile,
				"--known-hosts", filepath.Join(root, "known_hosts"),
				"--log", filepath.Join(root, "remote.log"),
				"--output", filepath.Join(root, "wrapper"),
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
