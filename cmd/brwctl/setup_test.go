package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// fakeRunner stands in for every external tool setup shells out to, so a test
// can assert the exact argv without launchctl or the claude CLI on the box.
// Commands succeed unless a prefix is listed in failing, which is how a test
// says "this probe finds nothing yet".
type fakeRunner struct {
	onPath  map[string]bool
	failing []string
	output  map[string]string
	calls   []string
}

func newFakeRunner(failing ...string) *fakeRunner {
	return &fakeRunner{onPath: map[string]bool{}, output: map[string]string{}, failing: failing}
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	joined := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, joined)
	for _, prefix := range f.failing {
		if strings.HasPrefix(joined, prefix) {
			return "", errors.New("nothing there")
		}
	}
	return f.output[joined], nil
}

func (f *fakeRunner) look(name string) (string, bool) {
	if f.onPath[name] {
		return "/usr/bin/" + name, true
	}
	return "", false
}

func (f *fakeRunner) called(prefix string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// newTestOptions builds a setup run confined to a temporary home, with a fake
// app install complete enough for doctor to pass its file checks.
func newTestOptions(t *testing.T, runner commandRunner, out *bytes.Buffer) setupOptions {
	t.Helper()
	home := t.TempDir()
	appDir := filepath.Join(home, "app")
	for _, rel := range []string{"bin/brwd", "bin/brwcheck", "bin/brw-devtools-mcp", "extension/manifest.json"} {
		path := filepath.Join(appDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	skills := filepath.Join(appDir, "skills", "brw")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# brw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The browser profile directory doctor looks for. UserDataDir in a
	// generated policy is ~/-relative, so point the policy at this home via an
	// explicit profile instead of relying on the real user's browser.
	return setupOptions{
		profileName: "chrome-profile",
		workspace:   "brw-chrome-profile",
		browser:     setup.BrowserChrome,
		transport:   setup.TransportBridge,
		mcpClient:   "claude",
		policyPath:  filepath.Join(home, ".config", "brw", "browser-profiles.json"),
		appDir:      appDir,
		httpPort:    setup.DefaultHTTPPort,
		home:        home,
		goos:        "darwin",
		out:         out,
		runner:      runner,
	}
}

// TestSetupFromZeroConfig is the first-time-user path end to end: no policy, no
// service, no registration. The assertions are the things a hand-written
// install script had to do.
func TestSetupFromZeroConfig(t *testing.T) {
	runner := newFakeRunner("defaults read", "launchctl print", "claude mcp get")
	runner.onPath["claude"] = true
	var out bytes.Buffer
	opts := newTestOptions(t, runner, &out)

	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}

	policy, found, err := setup.LoadPolicyFile(opts.policyPath)
	if err != nil || !found {
		t.Fatalf("policy not written: found=%v err=%v", found, err)
	}
	if _, err := policy.ResolveProfile(opts.workspace, ""); err != nil {
		t.Fatalf("generated policy cannot resolve its own profile: %v", err)
	}
	if _, err := policy.ResolveTransport(opts.workspace, ""); err != nil {
		t.Fatalf("generated policy cannot resolve a transport: %v", err)
	}

	plist := filepath.Join(opts.home, "Library", "LaunchAgents", "co.donworks.brwd.chrome-profile.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("LaunchAgent not written: %v", err)
	}
	for _, want := range []string{"--bridge", "127.0.0.1:17311", "co.donworks.brwd.chrome-profile"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("LaunchAgent missing %q:\n%s", want, data)
		}
	}

	if !runner.called("defaults write com.google.Chrome NSAppSleepDisabled") {
		t.Fatalf("App Nap was never disabled: %v", runner.calls)
	}
	if !runner.called("launchctl bootstrap") {
		t.Fatalf("LaunchAgent was never loaded: %v", runner.calls)
	}
	if !runner.called("claude mcp add -s user brw") {
		t.Fatalf("MCP server was never registered: %v", runner.calls)
	}
	for _, destination := range setup.SkillDestinations(opts.home) {
		if _, err := os.Stat(filepath.Join(destination, "SKILL.md")); err != nil {
			t.Fatalf("skill missing at %s: %v", destination, err)
		}
	}
	// Registration goes through the CLI: ~/.claude.json is rewritten by a
	// running Claude Code and must never be edited from here.
	if _, err := os.Stat(filepath.Join(opts.home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("setup touched ~/.claude.json: %v", err)
	}
	if strings.Contains(out.String(), "\n  "+statusFail) {
		t.Fatalf("a clean run reported a failure:\n%s", out.String())
	}
}

// TestSetupIsIdempotent locks the re-run contract end to end: the second run
// writes nothing, reloads nothing, and re-registers nothing.
func TestSetupIsIdempotent(t *testing.T) {
	runner := newFakeRunner("defaults read", "launchctl print", "claude mcp get")
	runner.onPath["claude"] = true
	var first bytes.Buffer
	opts := newTestOptions(t, runner, &first)
	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}
	policyBefore := readBytes(t, opts.policyPath)
	plist := filepath.Join(opts.home, "Library", "LaunchAgents", "co.donworks.brwd.chrome-profile.plist")
	plistBefore := readBytes(t, plist)

	// Second run: the job is now loaded, App Nap is set, and claude already
	// knows the server.
	second := newFakeRunner()
	second.onPath["claude"] = true
	second.output["defaults read com.google.Chrome NSAppSleepDisabled"] = "1"
	second.output["launchctl print gui/"+strconv.Itoa(os.Getuid())+"/co.donworks.brwd.chrome-profile"] = "state = running"
	second.output["claude mcp get brw"] = "brw: stdio"
	var rerun bytes.Buffer
	opts.runner = second
	opts.out = &rerun
	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(policyBefore, readBytes(t, opts.policyPath)) {
		t.Fatal("re-running setup rewrote the policy")
	}
	if !bytes.Equal(plistBefore, readBytes(t, plist)) {
		t.Fatal("re-running setup rewrote the LaunchAgent")
	}
	entries, err := filepath.Glob(opts.policyPath + ".bak.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a no-op re-run created backups: %v", entries)
	}
	for _, unwanted := range []string{"defaults write", "launchctl bootstrap", "launchctl bootout", "claude mcp add"} {
		if second.called(unwanted) {
			t.Fatalf("idempotent re-run performed %q: %v", unwanted, second.calls)
		}
	}
	text := rerun.String()
	for _, want := range []string{
		"is already complete; leaving it untouched",
		"already has NSAppSleepDisabled=YES",
		"is already loaded",
		`claude already has an MCP server named "brw"`,
		"is already current",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("re-run output missing %q:\n%s", want, text)
		}
	}
}

// TestSetupRefusesPreExistingLaunchAgent covers the developer machine that
// already carries a hand-made agent for the same profile.
func TestSetupRefusesPreExistingLaunchAgent(t *testing.T) {
	runner := newFakeRunner("defaults read")
	var out bytes.Buffer
	opts := newTestOptions(t, runner, &out)
	opts.mcpClient = "none"

	agentDir := filepath.Join(opts.home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	handMade := filepath.Join(agentDir, "co.acme.brw.chrome.plist")
	handMadeContent := `<?xml version="1.0"?><plist><dict>
  <key>Label</key><string>co.acme.brw.chrome</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/brwd</string>
    <string>--profile</string><string>chrome-profile</string>
    <string>--bridge</string>
  </array></dict></plist>`
	if err := os.WriteFile(handMade, []byte(handMadeContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}

	if got := readBytes(t, handMade); string(got) != handMadeContent {
		t.Fatal("setup modified the pre-existing LaunchAgent")
	}
	ours := filepath.Join(agentDir, "co.donworks.brwd.chrome-profile.plist")
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatalf("setup wrote its own agent despite the conflict: %v", err)
	}
	if runner.called("launchctl bootstrap") || runner.called("launchctl bootout") {
		t.Fatalf("setup touched launchctl despite the conflict: %v", runner.calls)
	}
	text := out.String()
	for _, want := range []string{
		statusRefuse,
		"co.acme.brw.chrome",
		"drives profile chrome-profile",
		"leaving the existing LaunchAgent alone",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal output missing %q:\n%s", want, text)
		}
	}
	// A refusal is a report, not a failure: the rest of setup still runs.
	if !strings.Contains(text, "[4/6] MCP client registration") {
		t.Fatalf("setup stopped after the refusal:\n%s", text)
	}
}

// TestSetupDryRunPerformsNothing is the diffability contract: every action is
// named, and none of them happens.
func TestSetupDryRunPerformsNothing(t *testing.T) {
	runner := newFakeRunner("defaults read", "launchctl print", "claude mcp get")
	runner.onPath["claude"] = true
	var out bytes.Buffer
	opts := newTestOptions(t, runner, &out)
	opts.dryRun = true

	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(opts.policyPath); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the policy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.home, "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Fatalf("dry run created the LaunchAgents directory: %v", err)
	}
	for _, destination := range setup.SkillDestinations(opts.home) {
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatalf("dry run installed the skill at %s", destination)
		}
	}
	// Read-only probes (defaults read, launchctl print, claude mcp get) still
	// run in a dry run: that is what makes the printed plan match what a real
	// run would do. Nothing that changes state may run.
	for _, unwanted := range []string{"defaults write", "launchctl bootstrap", "launchctl bootout", "launchctl kickstart", "claude mcp add", "codex mcp add", "schtasks", "systemctl"} {
		if runner.called(unwanted) {
			t.Fatalf("dry run performed %q: %v", unwanted, runner.calls)
		}
	}

	text := out.String()
	for _, want := range []string{
		"[1/6] profile policy",
		"[2/6] browser App Nap",
		"[3/6] background service",
		"[4/6] MCP client registration",
		"[5/6] agent skill",
		"[6/6] verify",
		"would   write " + opts.policyPath,
		"would   claude mcp add -s user brw",
		"still to do by hand:",
		"dry run: nothing above was performed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\n  "+statusDid) {
		t.Fatalf("dry run reported a performed action:\n%s", text)
	}
}

func TestSetupNoServicePrintsForegroundCommand(t *testing.T) {
	runner := newFakeRunner("defaults read")
	var out bytes.Buffer
	opts := newTestOptions(t, runner, &out)
	opts.noService = true
	opts.mcpClient = "none"
	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "--no-service; run the daemon in the foreground with:") {
		t.Fatalf("missing foreground instruction:\n%s", text)
	}
	if !strings.Contains(text, "--bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311") {
		t.Fatalf("foreground command is not the serviced one:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(opts.home, "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Fatal("--no-service still wrote a LaunchAgent")
	}
}

func TestSetupOptionValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*setupOptions)
		wantErr bool
		check   func(t *testing.T, o setupOptions)
	}{
		{
			name:   "defaults are derived from browser and lane",
			mutate: func(o *setupOptions) { o.profileName = ""; o.workspace = "" },
			check: func(t *testing.T, o setupOptions) {
				if o.profileName != "chrome-profile" || o.workspace != "brw-chrome-profile" {
					t.Fatalf("derived names = %q / %q", o.profileName, o.workspace)
				}
			},
		},
		{
			name:   "direct-cdp derives its own names",
			mutate: func(o *setupOptions) { o.profileName = ""; o.workspace = ""; o.transport = setup.TransportDirectCDP },
			check: func(t *testing.T, o setupOptions) {
				if o.profileName != "chrome-agent" || o.workspace != "brw-chrome-agent" {
					t.Fatalf("derived names = %q / %q", o.profileName, o.workspace)
				}
			},
		},
		{name: "unknown lane", mutate: func(o *setupOptions) { o.transport = "websocket" }, wantErr: true},
		{name: "unknown mcp client", mutate: func(o *setupOptions) { o.mcpClient = "cursor" }, wantErr: true},
		{name: "unknown browser", mutate: func(o *setupOptions) { o.browser = "safari" }, wantErr: true},
		{name: "port out of range", mutate: func(o *setupOptions) { o.httpPort = 70000 }, wantErr: true},
		{name: "port would overflow the bridge", mutate: func(o *setupOptions) { o.httpPort = 65535 }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := setupOptions{
				browser:   setup.BrowserChrome,
				transport: setup.TransportBridge,
				mcpClient: "claude",
				httpPort:  setup.DefaultHTTPPort,
				goos:      "darwin",
			}
			tc.mutate(&opts)
			err := opts.normalise()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, opts)
			}
		})
	}
}

// TestDetectBrowserPrefersAUsedBrowser: an installed-but-never-launched Chrome
// has no profile directory, so a policy bound to it cannot verify. A browser
// with real profiles wins.
func TestDetectBrowserPrefersAUsedBrowser(t *testing.T) {
	cases := []struct {
		name  string
		used  []string
		want  string
		files map[string]string
	}{
		{
			name: "chrome has been used",
			files: map[string]string{
				"Library/Application Support/Google/Chrome/Default/Preferences": "{}",
			},
			want: setup.BrowserChrome,
		},
		{
			name: "only chromium has been used",
			files: map[string]string{
				"Library/Application Support/Chromium/Profile 1/Preferences": "{}",
			},
			want: setup.BrowserChromium,
		},
		{
			name: "both used prefers chrome",
			files: map[string]string{
				"Library/Application Support/Google/Chrome/Default/Preferences": "{}",
				"Library/Application Support/Chromium/Default/Preferences":      "{}",
			},
			want: setup.BrowserChrome,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for name, content := range tc.files {
				path := filepath.Join(home, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := detectBrowser("darwin", home); got != tc.want {
				t.Fatalf("detectBrowser = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetupBindsAnExistingProfileDirectory: the generated policy must name a
// profile directory that is actually there, not always "Default".
func TestSetupBindsAnExistingProfileDirectory(t *testing.T) {
	runner := newFakeRunner("defaults read", "launchctl print", "claude mcp get")
	var out bytes.Buffer
	opts := newTestOptions(t, runner, &out)
	opts.mcpClient = "none"
	opts.noService = true
	chromeDir := filepath.Join(opts.home, "Library", "Application Support", "Google", "Chrome", "Profile 3")
	if err := os.MkdirAll(chromeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chromeDir, "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}
	policy, _, err := setup.LoadPolicyFile(opts.policyPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := policy.Find("chrome-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProfileDirectory != "Profile 3" {
		t.Fatalf("profile_directory = %q, want the directory that exists", profile.ProfileDirectory)
	}

	// An explicit flag still wins.
	opts.policyPath = filepath.Join(t.TempDir(), "explicit.json")
	opts.profileDirectory = "Profile 9"
	if _, err := runSetup(opts); err != nil {
		t.Fatal(err)
	}
	policy, _, err = setup.LoadPolicyFile(opts.policyPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err = policy.Find("chrome-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProfileDirectory != "Profile 9" {
		t.Fatalf("profile_directory = %q, want the explicit flag value", profile.ProfileDirectory)
	}
}

// TestDeriveMCPServerFromGeneratedPolicy is the regression the whole command
// exists for: `mcp-config` used to fail on a zero-config machine with
// "--transport is required when workspace has no default_transport".
func TestDeriveMCPServerFromGeneratedPolicy(t *testing.T) {
	policy, _ := setup.Merge(profilepolicy.Policy{}, setup.PolicyRequest{
		Workspace: "brw-chrome-profile",
		Profile:   "chrome-profile",
		Browser:   setup.BrowserChrome,
		Transport: setup.TransportBridge,
		BRWDPath:  "/opt/brw/bin/brwd",
		HTTPPort:  setup.DefaultHTTPPort,
		GOOS:      "darwin",
	})
	spec, err := deriveMCPServer(policy, mcpConfigRequest{
		Workspace:  "brw-chrome-profile",
		PolicyPath: "/home/someone/.config/brw/browser-profiles.json",
		Transport:  setup.LocalTransportName,
		Mode:       "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "brw" || spec.Command != "/opt/brw/bin/brwd" || spec.Mode != "bridge" {
		t.Fatalf("spec = %+v", spec)
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "--bridge") || !strings.Contains(joined, "--bridge-addr 127.0.0.1:17311") {
		t.Fatalf("args = %q", joined)
	}
	for key, want := range map[string]string{
		"BRW_WORKSPACE":      "brw-chrome-profile",
		"BRW_PROFILE":        "chrome-profile",
		"BRW_PROFILE_POLICY": "/home/someone/.config/brw/browser-profiles.json",
	} {
		if spec.Env[key] != want {
			t.Fatalf("env %s = %q, want %q", key, spec.Env[key], want)
		}
	}

	add := claudeAddArgs(spec)
	joinedAdd := strings.Join(add, " ")
	for _, want := range []string{
		"claude mcp add -s user brw",
		"-e BRW_PROFILE=chrome-profile",
		"-- /opt/brw/bin/brwd --bridge",
	} {
		if !strings.Contains(joinedAdd, want) {
			t.Fatalf("claude add args missing %q: %s", want, joinedAdd)
		}
	}
	codex := strings.Join(codexAddArgs(spec), " ")
	if !strings.Contains(codex, "codex mcp add brw --env BRW_PROFILE=chrome-profile") {
		t.Fatalf("codex add args = %s", codex)
	}
}

// TestDoctorReportContract pins the JSON keys existing consumers read and the
// two fields added for transport honesty.
func TestDoctorReportContract(t *testing.T) {
	home := t.TempDir()
	appDir := filepath.Join(home, "app")
	profileDir := filepath.Join(home, "Chrome", "Default")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "Preferences"), []byte(
		`{"extensions":{"settings":{"`+profilepolicy.DefaultBridgeExtensionID+`":{"state":1}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{
		Name:                   "chrome-profile",
		Kind:                   "chrome",
		UserDataDir:            filepath.Join(home, "Chrome"),
		ProfileDirectory:       "Default",
		ExtensionBridgeAllowed: true,
	}}}

	cases := []struct {
		name        string
		claudeJSON  string
		wantWarning string
	}{
		{name: "no claude config", wantWarning: ""},
		{name: "claude in chrome on", claudeJSON: `{"claudeInChromeDefaultEnabled": true}`, wantWarning: setup.ClaudeInChromeWarning},
		{name: "claude in chrome off", claudeJSON: `{"claudeInChromeDefaultEnabled": false}`, wantWarning: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := setup.ClaudeConfigPath(home)
			_ = os.Remove(configPath)
			if tc.claudeJSON != "" {
				if err := os.WriteFile(configPath, []byte(tc.claudeJSON), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			report, err := doctorReport(doctorRequest{
				Profile: "chrome-profile",
				AppDir:  appDir,
				Home:    home,
				Policy:  &policy,
			})
			if err != nil {
				t.Fatal(err)
			}

			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			// Keys that predate this change. Consumers read them by name.
			for _, key := range []string{
				"profile", "kind", "app_dir", "chrome_profile_dir",
				"bridge_extension_id", "bridge_extension_installed", "bridge_extension_source",
				"ok", "failures",
			} {
				if _, ok := decoded[key]; !ok {
					t.Fatalf("doctor JSON lost the %q key: %s", key, encoded)
				}
			}
			if report.Transport != setup.ResolvedExtensionBridge {
				t.Fatalf("transport = %q", report.Transport)
			}
			if report.Capabilities == nil || !strings.Contains(report.Capabilities.Lacks, "brw_open_incognito") {
				t.Fatalf("capabilities = %+v", report.Capabilities)
			}
			if report.BridgeExtensionInstalled == nil || !*report.BridgeExtensionInstalled {
				t.Fatal("extension in Preferences was not detected")
			}

			claudeWarnings := 0
			for _, warning := range report.Warnings {
				if warning.Name == setup.ClaudeInChromeWarning {
					claudeWarnings++
					if !strings.Contains(warning.Message, "/chrome") {
						t.Fatalf("warning must name the fix: %q", warning.Message)
					}
				}
			}
			want := 0
			if tc.wantWarning != "" {
				want = 1
			}
			if claudeWarnings != want {
				t.Fatalf("claude-in-chrome warnings = %d, want %d", claudeWarnings, want)
			}
		})
	}
}

// TestDoctorUsesDefaultBridgeExtensionID matches doctor to the daemon: an
// unconfigured bridge already trusts the published id, so a policy that simply
// omits bridge_extension_id must not fail verification.
func TestDoctorUsesDefaultBridgeExtensionID(t *testing.T) {
	home := t.TempDir()
	profileDir := filepath.Join(home, "Chrome", "Default")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "Preferences"), []byte(
		`{"extensions":{"settings":{"`+profilepolicy.DefaultBridgeExtensionID+`":{"state":1}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{
		Name:                   "chrome-profile",
		UserDataDir:            filepath.Join(home, "Chrome"),
		ProfileDirectory:       "Default",
		ExtensionBridgeAllowed: true,
	}}}
	report, err := doctorReport(doctorRequest{Profile: "chrome-profile", AppDir: filepath.Join(home, "app"), Home: home, Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	if report.BridgeExtensionID != profilepolicy.DefaultBridgeExtensionID {
		t.Fatalf("bridge_extension_id = %q, want the published default", report.BridgeExtensionID)
	}
	for _, failure := range report.Failures {
		if strings.Contains(failure, "bridge_extension_id is required") {
			t.Fatalf("doctor still fails a policy that omits the id: %v", report.Failures)
		}
	}
}

// TestDoctorAppDirPolicyCopyIsAWarning: a machine set up by `brwctl setup`
// legitimately has no policy copy inside the app directory, and that must not
// be a hard failure.
func TestDoctorAppDirPolicyCopyIsAWarning(t *testing.T) {
	home := t.TempDir()
	appDir := filepath.Join(home, "app")
	for _, rel := range []string{"bin/brwd", "bin/brwcheck", "bin/brw-devtools-mcp", "extension/manifest.json"} {
		path := filepath.Join(appDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	profileDir := filepath.Join(home, "chrome-agent")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{
		Name:             "chrome-agent",
		UserDataDir:      profileDir,
		DirectCDPAllowed: true,
	}}}
	report, err := doctorReport(doctorRequest{Profile: "chrome-agent", AppDir: appDir, Home: home, Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("doctor failed on a setup-configured machine: %v", report.Failures)
	}
	found := false
	for _, warning := range report.Warnings {
		if warning.Name == "app_dir_policy_copy_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing app-dir policy copy was not reported at all: %+v", report.Warnings)
	}
	if report.Transport != setup.ResolvedDirectCDP {
		t.Fatalf("transport = %q, want direct-cdp", report.Transport)
	}
	if report.Capabilities == nil || !strings.Contains(report.Capabilities.Has, "brw_cookies") {
		t.Fatalf("direct-cdp capabilities = %+v", report.Capabilities)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
