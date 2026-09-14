package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

const (
	fixtureWorkspace      = "brw-chrome-profile"
	fixtureProfile        = "chrome-profile"
	fixtureExtensionBuild = "1.4.0"
	fixtureChromeVersion  = "141.0.7390.55"
	fixtureChromePath     = "/usr/bin/google-chrome"
)

// doctorFixture is a whole brw machine on disk and on loopback: app payload,
// browser profile, policy, agent client config, daemon and bridge. Every check
// doctor makes has something real to look at, so a test breaks a machine by
// changing one thing and asserting on one check.
//
// GOOS is linux throughout: on that lane the browser binary is found through
// the runner's PATH lookup, which a test controls, rather than through whatever
// happens to be installed in /Applications on the machine running the test.
type doctorFixture struct {
	t          *testing.T
	home       string
	appDir     string
	profileDir string
	policyPath string
	runner     *fakeRunner
	health     daemonHealth
	status     bridgeStatus
	daemon     *httptest.Server
	bridge     *httptest.Server
	policy     profilepolicy.Policy
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	home := t.TempDir()
	fx := &doctorFixture{
		t:          t,
		home:       home,
		appDir:     filepath.Join(home, "app"),
		profileDir: filepath.Join(home, "browser", "Default"),
		policyPath: filepath.Join(home, "config", "browser-profiles.json"),
		runner:     newFakeRunner(),
	}
	fx.health = daemonHealth{
		OK:       true,
		Identity: brwidentity.Identity{Workspace: fixtureWorkspace, Profile: fixtureProfile},
	}
	fx.status.Connected = true
	fx.status.ConnectedAt = "2026-01-01T00:00:00Z"
	fx.status.Hello.Build = fixtureExtensionBuild

	fx.runner.onPath["google-chrome"] = true
	fx.runner.output[fixtureChromePath+" --version"] = "Google Chrome " + fixtureChromeVersion

	fx.writeFile(filepath.Join(fx.appDir, "bin", "brwd"), "brwd")
	fx.writeFile(filepath.Join(fx.appDir, "bin", "brwcheck"), "brwcheck")
	fx.writeFile(filepath.Join(fx.appDir, "bin", "brw-devtools-mcp"), "brw-devtools-mcp")
	fx.writeExtensionPayload(filepath.Join(fx.appDir, "extension"), fixtureExtensionBuild)
	fx.writeFile(filepath.Join(fx.profileDir, "Preferences"),
		`{"extensions":{"settings":{"`+profilepolicy.DefaultBridgeExtensionID+`":{"state":1}}}}`)
	fx.registerMCPServer(filepath.Join(fx.appDir, "bin", "brwd"))
	fx.installServiceUnit()

	fx.daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(fx.health)
	}))
	t.Cleanup(fx.daemon.Close)
	fx.bridge = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(fx.status)
	}))
	t.Cleanup(fx.bridge.Close)

	fx.policy = profilepolicy.Policy{
		WorkspaceBindings: []profilepolicy.WorkspaceBinding{{
			Workspace:        fixtureWorkspace,
			DefaultProfile:   fixtureProfile,
			DefaultTransport: setup.LocalTransportName,
		}},
		Profiles: []profilepolicy.Profile{{
			Name:                   fixtureProfile,
			Kind:                   setup.BrowserChrome,
			UserDataDir:            filepath.Join(home, "browser"),
			ProfileDirectory:       "Default",
			ExtensionBridgeAllowed: true,
			BridgeHTTPAddr:         fx.daemon.URL,
			BridgeWSAddr:           strings.TrimPrefix(fx.bridge.URL, "http://"),
		}},
		Transports: []profilepolicy.Transport{{
			Name:    setup.LocalTransportName,
			Kind:    "stdio",
			Command: filepath.Join(fx.appDir, "bin", "brwd"),
		}},
	}
	fx.writePolicy()
	return fx
}

func (fx *doctorFixture) writeFile(path, content string) {
	fx.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *doctorFixture) writeExtensionPayload(dir, version string) {
	fx.t.Helper()
	fx.writeFile(filepath.Join(dir, "manifest.json"), `{"name":"brw","version":"`+version+`"}`)
}

func (fx *doctorFixture) registerMCPServer(command string) {
	fx.t.Helper()
	config := map[string]any{"mcpServers": map[string]any{"brw": map[string]any{
		"command": command,
		"args":    []string{"--bridge", "--mcp", "--http", "off"},
	}}}
	encoded, err := json.Marshal(config)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.writeFile(setup.ClaudeConfigPath(fx.home), string(encoded))
}

func (fx *doctorFixture) installServiceUnit() {
	fx.t.Helper()
	params := setup.ServiceParams{GOOS: "linux", Profile: fixtureProfile, Home: fx.home}
	fx.writeFile(params.UnitPath(), "[Service]\n")
}

func (fx *doctorFixture) writePolicy() {
	fx.t.Helper()
	encoded, err := json.Marshal(fx.policy)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.writeFile(fx.policyPath, string(encoded))
}

func (fx *doctorFixture) report() doctorResult {
	fx.t.Helper()
	return doctorReport(doctorRequest{
		Workspace:  fixtureWorkspace,
		PolicyPath: fx.policyPath,
		AppDir:     fx.appDir,
		Home:       fx.home,
		GOOS:       "linux",
		Executable: filepath.Join(fx.appDir, "bin", "brwctl"),
		Runner:     fx.runner,
	})
}

func checkByName(t *testing.T, report doctorResult, name string) doctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("report has no %q check: %+v", name, report.Checks)
	return doctorCheck{}
}

// TestDoctorReportsAWorkingMachineAsGreen is the baseline every failure case
// below is measured against: with the daemon up, the bridge connected and the
// registration current, nothing is red.
func TestDoctorReportsAWorkingMachineAsGreen(t *testing.T) {
	fx := newDoctorFixture(t)
	report := fx.report()
	if !report.OK {
		t.Fatalf("green machine reported failures: %v", report.Failures)
	}
	for _, check := range report.Checks {
		if check.Status == checkFail {
			t.Fatalf("check %s is red on a green machine: %s", check.Name, check.Detail)
		}
	}
	if got := checkByName(t, report, "browser_binary"); !strings.Contains(got.Detail, fixtureChromeVersion) {
		t.Fatalf("browser version was not reported: %q", got.Detail)
	}
	if report.BrowserVersion != fixtureChromeVersion {
		t.Fatalf("browser_version = %q", report.BrowserVersion)
	}
	if got := checkByName(t, report, "bridge_connected"); got.Status != checkOK {
		t.Fatalf("bridge check = %+v", got)
	}
	if got := checkByName(t, report, "extension_version"); got.Status != checkOK {
		t.Fatalf("extension version check = %+v", got)
	}
	if report.Transport != setup.ResolvedExtensionBridge || report.Capabilities == nil {
		t.Fatalf("transport summary = %q / %+v", report.Transport, report.Capabilities)
	}
}

// TestDoctorNamesAFixForEveryBrokenCheck injects one fault at a time and
// asserts the check that owns it goes red with a command that addresses it. A
// red line an operator cannot act on is a red line they learn to skip.
func TestDoctorNamesAFixForEveryBrokenCheck(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(fx *doctorFixture)
		check   string
		wantFix string
		wantIn  string
	}{
		{
			name:    "daemon not running",
			break_:  func(fx *doctorFixture) { fx.daemon.Close() },
			check:   "daemon",
			wantFix: "systemctl --user restart brwd-chrome-profile.service",
			wantIn:  "nothing is listening",
		},
		{
			name: "daemon has no service unit to restart",
			break_: func(fx *doctorFixture) {
				fx.daemon.Close()
				params := setup.ServiceParams{GOOS: "linux", Profile: fixtureProfile, Home: fx.home}
				if err := os.Remove(params.UnitPath()); err != nil {
					fx.t.Fatal(err)
				}
			},
			check:   "daemon",
			wantFix: "brwctl setup --workspace " + fixtureWorkspace,
			wantIn:  "nothing is listening",
		},
		{
			name: "another workspace's daemon owns the port",
			break_: func(fx *doctorFixture) {
				fx.health.Identity = brwidentity.Identity{Workspace: "brw-other", Profile: "other-profile"}
			},
			check:   "daemon",
			wantFix: "brwctl daemons",
			wantIn:  "it serves",
		},
		{
			name: "policy file is missing",
			break_: func(fx *doctorFixture) {
				if err := os.Remove(fx.policyPath); err != nil {
					fx.t.Fatal(err)
				}
			},
			check:   "profile_policy",
			wantFix: "brwctl setup --profile-policy",
			wantIn:  "cannot read",
		},
		{
			name: "policy no longer defines the profile",
			break_: func(fx *doctorFixture) {
				// The binding still names chrome-profile; the profile it points
				// at has been renamed out from under it.
				fx.policy.Profiles[0].Name = "renamed-profile"
				fx.writePolicy()
			},
			check:   "profile_resolved",
			wantFix: "brwctl setup",
			wantIn:  "renamed-profile",
		},
		{
			name: "app payload is incomplete",
			break_: func(fx *doctorFixture) {
				if err := os.Remove(filepath.Join(fx.appDir, "bin", "brwd")); err != nil {
					fx.t.Fatal(err)
				}
			},
			check:   "app_files",
			wantFix: "install.sh",
			wantIn:  "missing",
		},
		{
			name:    "browser is not installed",
			break_:  func(fx *doctorFixture) { fx.runner.onPath["google-chrome"] = false },
			check:   "browser_binary",
			wantFix: "--user-data-dir",
			wantIn:  "Google Chrome is not installed",
		},
		{
			name: "browser profile has never been created",
			break_: func(fx *doctorFixture) {
				if err := os.RemoveAll(fx.profileDir); err != nil {
					fx.t.Fatal(err)
				}
			},
			check:   "browser_profile_dir",
			wantFix: "open Google Chrome once",
			wantIn:  "missing",
		},
		{
			name: "extension is not installed in the profile",
			break_: func(fx *doctorFixture) {
				fx.writeFile(filepath.Join(fx.profileDir, "Preferences"), `{"extensions":{"settings":{}}}`)
			},
			check:   "bridge_extension",
			wantFix: "chrome://extensions",
			wantIn:  "is not installed in",
		},
		{
			name: "extension is installed but has never connected",
			break_: func(fx *doctorFixture) {
				fx.status.Connected = false
				fx.status.DisconnectReason = "extension disconnected"
			},
			check:   "bridge_connected",
			wantFix: "chrome://extensions",
			wantIn:  "no extension has connected",
		},
		{
			name: "browser runs an older build than the installed payload",
			break_: func(fx *doctorFixture) {
				fx.status.Hello.Build = "1.1.0"
			},
			check:   "extension_version",
			wantFix: "chrome://extensions",
			wantIn:  "running build 1.1.0",
		},
		{
			name: "a per-profile extension copy fell behind",
			break_: func(fx *doctorFixture) {
				fx.writeExtensionPayload(filepath.Join(fx.appDir, "extension-work"), "1.1.0")
			},
			check:   "extension_version",
			wantFix: "brwctl upgrade --refresh-extensions",
			wantIn:  "extension-work",
		},
		{
			name: "Claude Code is installed and registers no brw server",
			break_: func(fx *doctorFixture) {
				fx.runner.onPath["claude"] = true
				fx.writeFile(setup.ClaudeConfigPath(fx.home), `{"mcpServers":{}}`)
			},
			check:   "mcp_registration",
			wantFix: "claude mcp add -s user brw",
			wantIn:  "registers no MCP server",
		},
		{
			name: "the extension presents a token this bridge does not know",
			break_: func(fx *doctorFixture) {
				fx.status.Connected = false
				fx.status.DisconnectReason = "handshake rejected: invalid handshake token"
			},
			check: "bridge_connected",
			// A reload re-reads the same wrong token from the same status URL,
			// so the fix has to name the setting that is wrong.
			wantFix: "Extension options: set Bridge URL to ws://",
			wantIn:  "handshake rejected",
		},
		{
			name: "the extension presents no token at all",
			break_: func(fx *doctorFixture) {
				fx.status.Connected = false
				fx.status.DisconnectReason = "handshake rejected: missing handshake token (BRW_BRIDGE_REQUIRE_TOKEN is set)"
			},
			check: "bridge_connected",
			// A build too old to read /status starts presenting one as soon as
			// the browser picks up the installed payload.
			wantFix: "click Reload under brw",
			wantIn:  "handshake rejected",
		},
		{
			name:   "the configured transport has no live bridge",
			break_: func(fx *doctorFixture) { fx.bridge.Close() },
			check:  "transport",
			// The lane is only as usable as the check it depends on.
			wantFix: "systemctl --user restart brwd-chrome-profile.service",
			wantIn:  "configured but not live",
		},
		{
			name:    "the configured transport has no live daemon",
			break_:  func(fx *doctorFixture) { fx.daemon.Close() },
			check:   "transport",
			wantFix: "systemctl --user restart brwd-chrome-profile.service",
			wantIn:  "configured but not live",
		},
		{
			name: "the registration points at a path that no longer exists",
			break_: func(fx *doctorFixture) {
				fx.registerMCPServer(filepath.Join(fx.home, "old-install", "bin", "brwd"))
			},
			check:   "mcp_registration",
			wantFix: "claude mcp remove -s user brw",
			wantIn:  "does not exist",
		},
		{
			name: "the registration points at another install's binary",
			break_: func(fx *doctorFixture) {
				stale := filepath.Join(fx.home, "old-install", "bin", "brwd")
				fx.writeFile(stale, "brwd")
				fx.registerMCPServer(stale)
			},
			check:   "mcp_registration",
			wantFix: "claude mcp remove -s user brw",
			wantIn:  "but this install's daemon is",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t)
			tc.break_(fx)
			report := fx.report()

			if report.OK {
				t.Fatalf("a broken machine reported OK: %+v", report.Checks)
			}
			got := checkByName(t, report, tc.check)
			if got.Status != checkFail {
				t.Fatalf("check %s = %+v, want a failure", tc.check, got)
			}
			if !strings.Contains(got.Detail, tc.wantIn) {
				t.Fatalf("check %s detail %q does not name the problem (%q)", tc.check, got.Detail, tc.wantIn)
			}
			if !strings.Contains(got.Fix, tc.wantFix) {
				t.Fatalf("check %s fix %q is not the expected command (%q)", tc.check, got.Fix, tc.wantFix)
			}
			// The whole point of the fix column: nothing may be red without one.
			for _, check := range report.Checks {
				if check.Status == checkFail && strings.TrimSpace(check.Fix) == "" {
					t.Fatalf("check %s is red with no fix command: %+v", check.Name, check)
				}
			}
			if len(report.Failures) == 0 {
				t.Fatal("failures list is empty on a failing report")
			}
		})
	}
}

// TestDoctorAcceptsAMachineThatRegistersNothing: `brwctl setup --mcp-client
// none` prints the server config for the operator to paste into a client brw
// cannot read, so an absent registration is what they asked for. Reporting it
// red sends them to re-register a client they chose not to use, and exits 1 on
// a machine that works.
func TestDoctorAcceptsAMachineThatRegistersNothing(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.policy.MCPClient = "none"
	fx.writePolicy()
	fx.runner.onPath["claude"] = true
	fx.writeFile(setup.ClaudeConfigPath(fx.home), `{"mcpServers":{}}`)

	report := fx.report()
	got := checkByName(t, report, "mcp_registration")
	if got.Status != checkWarn {
		t.Fatalf("mcp_registration = %+v, want a warning", got)
	}
	if !report.OK {
		t.Fatalf("a machine set up with --mcp-client none reported failures: %v", report.Failures)
	}
}

// TestDoctorJudgesTheTransportOnTheResolvedLane: a hand-edited policy can allow
// both lanes, and such a profile runs on direct CDP. A dead bridge is then not
// what stops it carrying a call, so it must not be reported as the transport's
// blocker.
func TestDoctorJudgesTheTransportOnTheResolvedLane(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.policy.Profiles[0].DirectCDPAllowed = true
	fx.writePolicy()
	fx.bridge.Close()

	report := fx.report()
	if report.Transport != setup.ResolvedDirectCDP {
		t.Fatalf("transport = %q, want %q", report.Transport, setup.ResolvedDirectCDP)
	}
	if got := checkByName(t, report, "transport"); got.Status != checkOK {
		t.Fatalf("transport check = %+v, want OK: the bridge is not this lane", got)
	}
}

// TestDoctorExitsNonZeroOnFailure is the contract a wrapper script gates on.
func TestDoctorExitsNonZeroOnFailure(t *testing.T) {
	fx := newDoctorFixture(t)
	var out bytes.Buffer
	if err := reportDoctor(&out, fx.report(), false); err != nil {
		t.Fatalf("green machine returned an error: %v", err)
	}

	fx.daemon.Close()
	out.Reset()
	err := reportDoctor(&out, fx.report(), false)
	if err == nil {
		t.Fatal("a failing report exited zero")
	}
	text := out.String()
	if !strings.Contains(text, "run: systemctl --user restart brwd-chrome-profile.service") {
		t.Fatalf("rendered report does not print the fix command:\n%s", text)
	}
	if !strings.Contains(text, "check(s) failed") {
		t.Fatalf("rendered report does not say what failed:\n%s", text)
	}
}

// TestDoctorJSONSchemaIsStable pins the --json contract: the key set of the
// document, the key set of one check, and the closed list of check names. A
// consumer reads these by name, so a field that appears or vanishes without a
// deliberate change here is a break.
func TestDoctorJSONSchemaIsStable(t *testing.T) {
	t.Run("a machine with one broken check", func(t *testing.T) {
		fx := newDoctorFixture(t)
		// One broken thing, so the failure-side keys are populated too.
		fx.status.Hello.Build = "1.1.0"
		assertDoctorSchema(t, fx.report(), true)
	})
	// Nothing resolvable at all: every check still has to report, as a skip.
	t.Run("a machine with no readable policy", func(t *testing.T) {
		fx := newDoctorFixture(t)
		if err := os.Remove(fx.policyPath); err != nil {
			t.Fatal(err)
		}
		assertDoctorSchema(t, fx.report(), false)
	})
	// Through the command, not doctorReport: a fresh machine with no policy to
	// discover is where --json is read most, and where a hand-built result
	// would quietly emit a different document.
	t.Run("the command's own unresolvable-policy path", func(t *testing.T) {
		dir := t.TempDir()
		// The command reads its own environment and PATH, and this test asserts
		// the document's shape rather than the machine it ran on: point all of
		// it at an empty directory so no agent client, browser or policy of the
		// operator's takes part.
		t.Setenv("BRW_PROFILE", "")
		t.Setenv("BRW_WORKSPACE", "")
		t.Setenv("BRW_PROFILE_POLICY", "")
		t.Setenv("HOME", dir)
		t.Setenv("PATH", dir)
		out, err := captureStdout(t, func() error {
			return doctor([]string{"--json",
				"--profile-policy", filepath.Join(dir, "missing", "browser-profiles.json"),
				"--app-dir", filepath.Join(dir, "app")})
		})
		if err == nil {
			t.Fatal("a machine with no readable policy exited zero")
		}
		assertDoctorSchemaJSON(t, out, false)
	})
}

// captureStdout runs fn with os.Stdout redirected, so a command that prints its
// report can be asserted on as the operator receives it.
func captureStdout(t *testing.T, fn func() error) ([]byte, error) {
	t.Helper()
	read, write, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	saved := os.Stdout
	os.Stdout = write
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(read)
		done <- data
	}()
	err := fn()
	os.Stdout = saved
	if closeErr := write.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return <-done, err
}

func assertDoctorSchema(t *testing.T, report doctorResult, requireAll bool) {
	t.Helper()
	var out bytes.Buffer
	if err := reportDoctor(&out, report, true); err == nil {
		t.Fatal("expected a non-zero exit for a report with a failing check")
	}
	assertDoctorSchemaJSON(t, out.Bytes(), requireAll)
}

func assertDoctorSchemaJSON(t *testing.T, raw []byte, requireAll bool) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, raw)
	}

	// Documented keys. The always set has no omitempty and must be present in
	// every report, however broken the machine; the rest appear once the thing
	// they describe has been resolved.
	always := []string{"profile", "kind", "app_dir", "chrome_profile_dir", "checks", "ok"}
	allowed := map[string]bool{
		"profile_policy_path": true, "bridge_extension_id": true, "bridge_extension_installed": true,
		"bridge_extension_source": true, "transport": true, "capabilities": true,
		"daemon_http_url": true, "bridge_ws_addr": true, "browser_executable": true,
		"browser_version": true, "extension_payload_version": true, "extension_loaded_version": true,
		"warnings": true, "failures": true,
	}
	for _, key := range always {
		allowed[key] = true
		if _, ok := decoded[key]; !ok {
			t.Fatalf("--json lost the %q key: %s", key, raw)
		}
	}
	for key := range decoded {
		if !allowed[key] {
			t.Fatalf("--json grew an undocumented key %q; add it to the schema test deliberately", key)
		}
	}
	if requireAll {
		for key := range allowed {
			if _, ok := decoded[key]; !ok {
				t.Fatalf("--json lost the %q key on a fully resolved machine: %s", key, raw)
			}
		}
	}

	checks, ok := decoded["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatalf("checks is not a populated array: %v", decoded["checks"])
	}
	known := map[string]bool{}
	for _, name := range doctorCheckNames {
		known[name] = true
	}
	seen := map[string]bool{}
	for _, raw := range checks {
		check, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("check is not an object: %v", raw)
		}
		for key := range check {
			switch key {
			case "name", "title", "status", "detail", "fix":
			default:
				t.Fatalf("check grew an undocumented key %q", key)
			}
		}
		for _, key := range []string{"name", "title", "status", "detail"} {
			if _, ok := check[key]; !ok {
				t.Fatalf("check %v is missing the %q key", check, key)
			}
		}
		name, _ := check["name"].(string)
		if !known[name] {
			t.Fatalf("check %q is not in doctorCheckNames", name)
		}
		if seen[name] {
			t.Fatalf("check %q was reported twice", name)
		}
		seen[name] = true
		status, _ := check["status"].(string)
		switch status {
		case checkOK, checkWarn, checkFail, checkSkip:
		default:
			t.Fatalf("check %q has status %q, which is not one of the four", name, status)
		}
	}
	for _, name := range doctorCheckNames {
		if !seen[name] {
			t.Fatalf("check %q was never reported; doctorCheckNames and the report disagree", name)
		}
	}
}

// TestDoctorAcceptsAMachineRegisteredWithAnotherClient: setup supports
// --mcp-client codex and --mcp-client none, and ~/.claude.json exists on any
// machine Claude Code has ever run on. Reporting either of those red sends the
// operator to re-register a client they deliberately did not use.
func TestDoctorAcceptsAMachineRegisteredWithAnotherClient(t *testing.T) {
	cases := []struct {
		name       string
		machine    func(fx *doctorFixture)
		wantStatus string
		wantIn     string
	}{
		{
			name: "registered with codex only",
			machine: func(fx *doctorFixture) {
				fx.runner.onPath["codex"] = true
				fx.writeFile(setup.ClaudeConfigPath(fx.home), `{"mcpServers":{}}`)
			},
			wantStatus: checkOK,
			wantIn:     "codex",
		},
		{
			name: "no agent client CLI on this machine",
			machine: func(fx *doctorFixture) {
				fx.writeFile(setup.ClaudeConfigPath(fx.home), `{"mcpServers":{}}`)
			},
			wantStatus: checkWarn,
			wantIn:     "no agent client CLI is on PATH",
		},
		{
			name: "codex is installed but has no brw server either",
			machine: func(fx *doctorFixture) {
				fx.runner.onPath["claude"] = true
				fx.runner.onPath["codex"] = true
				fx.runner.failing = append(fx.runner.failing, "codex mcp get")
				fx.writeFile(setup.ClaudeConfigPath(fx.home), `{"mcpServers":{}}`)
			},
			wantStatus: checkFail,
			wantIn:     "the codex CLI has no brw server either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t)
			tc.machine(fx)
			report := fx.report()

			got := checkByName(t, report, "mcp_registration")
			if got.Status != tc.wantStatus {
				t.Fatalf("mcp_registration = %+v, want status %q", got, tc.wantStatus)
			}
			if !strings.Contains(got.Detail, tc.wantIn) {
				t.Fatalf("mcp_registration detail %q does not name %q", got.Detail, tc.wantIn)
			}
			if report.OK == (tc.wantStatus == checkFail) {
				t.Fatalf("report.OK = %v for a %s check: %v", report.OK, tc.wantStatus, report.Failures)
			}
		})
	}
}

// TestDoctorSkipsLiveChecksForSetup: `brwctl setup` verifies before the operator
// has loaded the extension, so the probes must be skipped rather than red.
func TestDoctorSkipsLiveChecksForSetup(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.daemon.Close()
	fx.bridge.Close()
	report := doctorReport(doctorRequest{
		Workspace:      fixtureWorkspace,
		PolicyPath:     fx.policyPath,
		AppDir:         fx.appDir,
		Home:           fx.home,
		GOOS:           "linux",
		Executable:     filepath.Join(fx.appDir, "bin", "brwctl"),
		Runner:         fx.runner,
		SkipLiveChecks: true,
	})
	if !report.OK {
		t.Fatalf("skipping live checks still failed: %v", report.Failures)
	}
	for _, name := range []string{"daemon", "bridge_connected", "transport"} {
		if got := checkByName(t, report, name); got.Status != checkSkip {
			t.Fatalf("check %s = %+v, want skipped", name, got)
		}
	}
}

// TestDoctorDirectCDPProfileSkipsEveryExtensionCheck: a direct-CDP install has
// no extension at all, and reporting its absence as a fault would send the
// operator to fix something that is not broken.
func TestDoctorDirectCDPProfileSkipsEveryExtensionCheck(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.policy.Profiles[0].ExtensionBridgeAllowed = false
	fx.policy.Profiles[0].DirectCDPAllowed = true
	fx.writePolicy()

	report := fx.report()
	if !report.OK {
		t.Fatalf("direct-CDP machine reported failures: %v", report.Failures)
	}
	for _, name := range []string{"bridge_extension", "bridge_connected", "extension_version"} {
		if got := checkByName(t, report, name); got.Status != checkSkip {
			t.Fatalf("check %s = %+v, want skipped on direct CDP", name, got)
		}
	}
	if report.Transport != setup.ResolvedDirectCDP {
		t.Fatalf("transport = %q", report.Transport)
	}
	if got := checkByName(t, report, "transport"); !strings.Contains(got.Detail, "brw_cookies") {
		t.Fatalf("transport summary does not name the direct-CDP capabilities: %q", got.Detail)
	}
}
