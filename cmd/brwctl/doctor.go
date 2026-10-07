package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

var doctorCheckNames = []string{
	"profile_policy",
	"profile_resolved",
	"app_files",
	"browser_binary",
	"browser_profile_dir",
	"bridge_extension",
	"daemon",
	"bridge_connected",
	"bridge_config",
	"extension_version",
	"mcp_registration",
	"claude_in_chrome",
	"chrome_opt_in",
	"transport",
}

type doctorCheck struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctorWarning struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type doctorResult struct {
	Profile                  string              `json:"profile"`
	Kind                     string              `json:"kind"`
	AppDir                   string              `json:"app_dir"`
	ProfilePolicyPath        string              `json:"profile_policy_path,omitempty"`
	ChromeProfileDir         string              `json:"chrome_profile_dir"`
	BridgeExtensionID        string              `json:"bridge_extension_id,omitempty"`
	BridgeExtensionInstalled *bool               `json:"bridge_extension_installed,omitempty"`
	BridgeExtensionSource    string              `json:"bridge_extension_source,omitempty"`
	Transport                string              `json:"transport,omitempty"`
	Capabilities             *setup.Capabilities `json:"capabilities,omitempty"`
	ChromeOptIn              *doctorChromeOptIn  `json:"chrome_opt_in,omitempty"`
	DaemonHTTPURL            string              `json:"daemon_http_url,omitempty"`
	BridgeWSAddr             string              `json:"bridge_ws_addr,omitempty"`
	BrowserExecutable        string              `json:"browser_executable,omitempty"`
	BrowserVersion           string              `json:"browser_version,omitempty"`
	ExtensionPayloadVersion  string              `json:"extension_payload_version,omitempty"`
	ExtensionLoadedVersion   string              `json:"extension_loaded_version,omitempty"`
	Checks                   []doctorCheck       `json:"checks"`
	Warnings                 []doctorWarning     `json:"warnings,omitempty"`
	OK                       bool                `json:"ok"`
	Failures                 []string            `json:"failures,omitempty"`
}

type doctorRequest struct {
	Workspace  string
	Profile    string
	PolicyPath string
	AppDir     string
	Home       string
	GOOS       string
	Policy     *profilepolicy.Policy
	// Executable is the running brwctl.
	Executable string
	// Version is the installed build, the one a daemon should be running.
	Version string
	Runner  commandRunner
	Timeout time.Duration
	// SkipLiveChecks leaves the daemon and the bridge unprobed.
	SkipLiveChecks bool
	// ResolveError is why the caller could not name a workspace itself.
	ResolveError error
	// ChromeOptInUserDataDir overrides where the Chrome remote-debugging opt-in endpoint is looked for.
	ChromeOptInUserDataDir string
}

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var profileName, workspaceName, policyPath, appDir string
	var asJSON bool
	var timeout time.Duration
	fs.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace profile name")
	fs.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&appDir, "app-dir", defaultAppDir(), "brw app install directory")
	fs.BoolVar(&asJSON, "json", false, "print the full report as JSON instead of a readable table")
	fs.DurationVar(&timeout, "timeout", 3*time.Second, "per-probe timeout for the daemon and the bridge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var resolveErr error
	if profileName == "" && workspaceName == "" {

		workspaceName, resolveErr = soleWorkspace(policyPath)
	}
	home, _ := os.UserHomeDir()
	executable, _ := os.Executable()
	report := doctorReport(doctorRequest{
		Workspace:    workspaceName,
		Profile:      profileName,
		PolicyPath:   policyPath,
		AppDir:       appDir,
		Home:         home,
		Executable:   executable,
		Version:      mcp.Version,
		Timeout:      timeout,
		ResolveError: resolveErr,
	})
	return reportDoctor(os.Stdout, report, asJSON)
}

func reportDoctor(w io.Writer, report doctorResult, asJSON bool) error {
	if asJSON {
		if err := writeJSON(w, report); err != nil {
			return err
		}
	} else {
		renderDoctor(w, report)
	}
	if report.OK {
		return nil
	}
	return fmt.Errorf("%d doctor check(s) failed; run the commands printed above", len(report.Failures))
}

func soleWorkspace(policyPath string) (string, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return "", fmt.Errorf("--profile or --workspace is required (could not read the profile policy: %w)", err)
	}
	switch len(policy.WorkspaceBindings) {
	case 1:
		return policy.WorkspaceBindings[0].Workspace, nil
	case 0:
		return "", errors.New("--profile or --workspace is required; the profile policy binds no workspaces")
	default:
		names := make([]string, 0, len(policy.WorkspaceBindings))
		for _, binding := range policy.WorkspaceBindings {
			names = append(names, binding.Workspace)
		}
		return "", fmt.Errorf("--workspace is required; the profile policy binds %s", strings.Join(names, ", "))
	}
}

type doctorRun struct {
	req      doctorRequest
	result   doctorResult
	policy   profilepolicy.Policy
	profile  profilepolicy.Profile
	resolved bool
	client   *http.Client

	bridge *bridgeStatus

	health *daemonHealth
}

func doctorReport(req doctorRequest) doctorResult {
	if req.GOOS == "" {
		req.GOOS = runtime.GOOS
	}
	if req.Runner == nil {
		req.Runner = execRunner{}
	}
	if req.Timeout <= 0 {
		req.Timeout = 3 * time.Second
	}
	d := &doctorRun{
		req:    req,
		result: doctorResult{AppDir: req.AppDir, ProfilePolicyPath: req.PolicyPath},
		client: doctorClient(req.Timeout),
	}
	d.checkPolicy()
	d.checkAppFiles()
	d.checkBrowserBinary()
	d.checkProfileDir()
	d.checkBridgeExtension()
	d.checkDaemon()
	d.checkBridgeConnected()
	d.checkBridgeConfig()
	d.checkExtensionVersion()
	d.checkMCPRegistration()
	d.checkClaudeInChrome()
	d.checkChromeOptIn()
	d.checkTransport()
	return d.finish()
}

func (d *doctorRun) add(status, name, title, detail, fix string) {
	d.result.Checks = append(d.result.Checks, doctorCheck{
		Name: name, Title: title, Status: status, Detail: detail, Fix: fix,
	})
}

func (d *doctorRun) finish() doctorResult {
	var failures []string
	for _, check := range d.result.Checks {
		if check.Status == checkFail {
			failures = append(failures, check.Name+": "+check.Detail)
		}
	}
	d.result.OK = len(failures) == 0
	d.result.Failures = failures
	return d.result
}

func (d *doctorRun) workspaceFlag() string {
	if d.req.Workspace != "" {
		return " --workspace " + d.req.Workspace
	}
	if d.req.Profile != "" {
		return " --profile " + d.req.Profile
	}
	return ""
}

func (d *doctorRun) checkPolicy() {
	if d.req.Policy != nil {
		d.policy = *d.req.Policy
		detail := "using the policy setup merged in memory"
		if d.req.PolicyPath != "" {
			detail += ", destined for " + d.req.PolicyPath
		}
		d.add(checkOK, "profile_policy", "profile policy", detail, "")
		d.resolveProfile()
		return
	}
	path := d.req.PolicyPath
	if path == "" {
		discovered, err := profilepolicy.Discover("")
		if err != nil {
			d.add(checkFail, "profile_policy", "profile policy", err.Error(), "brwctl setup")
			d.add(checkSkip, "profile_resolved", "profile", "no policy to resolve a profile from", "")
			return
		}
		path = discovered
	}
	d.result.ProfilePolicyPath = path
	info, err := os.Stat(path)
	if err != nil {
		d.add(checkFail, "profile_policy", "profile policy", "cannot read "+path+": "+err.Error(), "brwctl setup --profile-policy "+path)
		d.add(checkSkip, "profile_resolved", "profile", "no policy to resolve a profile from", "")
		return
	}
	policy, err := profilepolicy.Load(path)
	if err != nil {
		d.add(checkFail, "profile_policy", "profile policy", path+" is not a readable policy: "+err.Error(), "brwctl setup --profile-policy "+path)
		d.add(checkSkip, "profile_resolved", "profile", "no policy to resolve a profile from", "")
		return
	}
	d.policy = policy
	d.add(checkOK, "profile_policy", "profile policy",
		fmt.Sprintf("%s (mode %04o, %d profile(s))", path, info.Mode().Perm(), len(policy.Profiles)), "")
	d.resolveProfile()
}

func (d *doctorRun) resolveProfile() {
	if d.req.ResolveError != nil {
		d.add(checkFail, "profile_resolved", "profile", d.req.ResolveError.Error(),
			"brwctl doctor --workspace <workspace>")
		return
	}
	profile, err := d.policy.ResolveProfile(d.req.Workspace, d.req.Profile)
	if err != nil {
		names := make([]string, 0, len(d.policy.Profiles))
		for _, candidate := range d.policy.Profiles {
			names = append(names, candidate.Name)
		}
		detail := err.Error()
		if len(names) > 0 {
			detail += "; the policy defines " + strings.Join(names, ", ")
		}
		d.add(checkFail, "profile_resolved", "profile", detail, "brwctl setup")
		return
	}
	d.profile = profile
	d.resolved = true
	d.result.Profile = profile.Name
	d.result.Kind = profile.Kind
	d.result.DaemonHTTPURL = discovery.HTTPURL(profile)
	if profile.ExtensionBridgeAllowed {
		d.result.BridgeWSAddr = discovery.WSAddr(profile)
	}
	d.add(checkOK, "profile_resolved", "profile",
		fmt.Sprintf("%s (%s), user data %s", profile.Name, setup.ResolvedTransport(profile), profile.UserDataDir), "")
}

func (d *doctorRun) checkAppFiles() {
	var missing []string
	for _, rel := range []string{
		"bin/brwd",
		"bin/brwcheck",
		"bin/brw-devtools-mcp",
		"extension/manifest.json",
	} {
		path := filepath.Join(d.req.AppDir, rel)
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		d.add(checkFail, "app_files", "app files", "missing "+strings.Join(missing, ", "),
			"curl -fsSL "+installScriptURL+" | sh")
	} else {
		d.add(checkOK, "app_files", "app files", "brwd, brwcheck, brw-devtools-mcp and the extension payload are in "+d.req.AppDir, "")
	}

	appPolicy := filepath.Join(d.req.AppDir, "config", "browser-profiles.json")
	if _, err := os.Stat(appPolicy); err != nil {
		d.result.Warnings = append(d.result.Warnings, doctorWarning{
			Name:    "app_dir_policy_copy_missing",
			Message: "no policy copy at " + appPolicy + "; the policy in use is " + d.result.ProfilePolicyPath,
			Detail:  "only needed when pushing this install to another machine",
		})
	}
}

func (d *doctorRun) checkBrowserBinary() {
	if !d.resolved {
		d.add(checkSkip, "browser_binary", "browser binary", "no profile resolved", "")
		return
	}
	browser, known := setup.LookupBrowser(d.profile.Kind)
	if !known {
		d.add(checkSkip, "browser_binary", "browser binary",
			fmt.Sprintf("profile kind %q is not one brw has install locations for; nothing to check", d.profile.Kind), "")
		return
	}
	exe := setup.BrowserExecutable(d.req.GOOS, browser, d.req.Runner.look)
	if exe == "" {
		d.add(checkFail, "browser_binary", "browser binary",
			browser.DisplayName+" is not installed where brw looks for it",
			"brwctl setup --browser <name> --user-data-dir <path>")
		return
	}
	d.result.BrowserExecutable = exe
	out, err := d.req.Runner.run(exe, "--version")
	version := setup.ParseBrowserVersion(out)
	if err != nil || version == "" {
		d.add(checkWarn, "browser_binary", "browser binary",
			exe+" did not report a version (it is installed, so brw can still drive it)", "")
		return
	}
	d.result.BrowserVersion = version
	d.add(checkOK, "browser_binary", "browser binary", browser.DisplayName+" "+version+" at "+exe, "")
}

func (d *doctorRun) checkProfileDir() {
	if !d.resolved {
		d.add(checkSkip, "browser_profile_dir", "browser profile directory", "no profile resolved", "")
		return
	}
	dir := filepath.Join(profilepolicy.ExpandPath(d.profile.UserDataDir), d.profile.ProfileDirectory)
	d.result.ChromeProfileDir = dir
	if _, err := os.Stat(dir); err != nil {
		d.add(checkFail, "browser_profile_dir", "browser profile directory",
			"missing "+dir+"; the browser has never created this profile",
			"open "+setup.BrowserDisplayName(d.profile.Kind)+" once, then re-run brwctl doctor"+d.workspaceFlag())
		return
	}
	d.add(checkOK, "browser_profile_dir", "browser profile directory", dir, "")
}

func (d *doctorRun) checkBridgeExtension() {
	if !d.resolved {
		d.add(checkSkip, "bridge_extension", "brw extension installed", "no profile resolved", "")
		return
	}
	if !d.profile.ExtensionBridgeAllowed {
		d.add(checkSkip, "bridge_extension", "brw extension installed", "direct-CDP profile: no extension is involved", "")
		return
	}

	id := d.profile.BridgeExtensionID
	if id == "" {
		id = profilepolicy.DefaultBridgeExtensionID
	}
	d.result.BridgeExtensionID = id
	installed, source, err := chromeExtensionInstalled(d.result.ChromeProfileDir, id)
	d.result.BridgeExtensionInstalled = &installed
	d.result.BridgeExtensionSource = source
	if err != nil {
		d.add(checkFail, "bridge_extension", "brw extension installed", err.Error(),
			"brwctl doctor"+d.workspaceFlag())
		return
	}
	if !installed {
		d.add(checkFail, "bridge_extension", "brw extension installed",
			"extension "+id+" is not installed in "+d.result.ChromeProfileDir,
			d.loadUnpackedCommand())
		return
	}
	d.add(checkOK, "bridge_extension", "brw extension installed", id+" is present in "+source, "")
}

func (d *doctorRun) extensionsPageCommand(note string) string {
	kind := d.browserKind()
	if d.req.GOOS == "darwin" {
		if kind == "" {
			return "open chrome://extensions in the profile's browser   # " + note
		}
		return fmt.Sprintf("open -a %q chrome://extensions   # %s", setup.BrowserDisplayName(kind), note)
	}
	exe := d.result.BrowserExecutable
	if exe == "" {
		exe = kind
	}
	return exe + " chrome://extensions   # " + note
}

func (d *doctorRun) browserKind() string {
	if d.profile.Kind != "" {
		return d.profile.Kind
	}
	dir := filepath.Clean(d.profile.UserDataDir)
	for _, name := range setup.BrowserNames() {
		known := setup.BrowserUserDataDir(d.req.GOOS, name)
		if rest, ok := strings.CutPrefix(known, "~/"); ok {
			known = filepath.Join(d.req.Home, rest)
		}
		if known != "" && filepath.Clean(known) == dir {
			return name
		}
	}
	return ""
}

func (d *doctorRun) loadUnpackedCommand() string {
	return d.extensionsPageCommand("Developer mode, Load unpacked, select " + filepath.Join(d.req.AppDir, "extension"))
}

func (d *doctorRun) reloadExtensionCommand() string {
	return d.extensionsPageCommand("click Reload under brw")
}

func (d *doctorRun) bridgeSettingsCommand(addr string) string {
	return d.extensionsPageCommand(fmt.Sprintf(
		"brw > Details > Extension options: set Bridge URL to ws://%s/extension and Status URL to http://%s/status",
		addr, addr))
}

func (d *doctorRun) serviceParams() setup.ServiceParams {
	return setup.ServiceParams{
		GOOS:      d.req.GOOS,
		Workspace: d.req.Workspace,
		Profile:   d.result.Profile,
		Home:      d.req.Home,
	}
}

func (d *doctorRun) serviceRestartCommand() string {
	params := d.serviceParams()
	if _, err := os.Stat(params.UnitPath()); err == nil {
		return serviceRestartCommand(d.req.GOOS, params)
	}
	brwd := filepath.Join(d.req.AppDir, "bin", "brwd")
	for _, unit := range brwdServiceUnits(d.req.GOOS, d.req.Home, brwd) {
		if unitProfile(unit) == d.result.Profile {
			return setup.Command(serviceRestartArgsForLabel(d.req.GOOS, unit.Label))
		}
	}
	return "brwctl setup" + d.workspaceFlag()
}

func serviceRestartCommand(goos string, params setup.ServiceParams) string {
	return setup.Command(serviceRestartArgs(goos, params))
}

func (d *doctorRun) checkDaemon() {
	if !d.resolved {
		d.add(checkSkip, "daemon", "daemon", "no profile resolved", "")
		return
	}
	if d.req.SkipLiveChecks {
		d.add(checkSkip, "daemon", "daemon", "live checks skipped; probe with: brwctl doctor"+d.workspaceFlag(), "")
		return
	}
	url := d.result.DaemonHTTPURL
	health, err := probeDaemonHealth(d.client, url)
	if err != nil {
		d.add(checkFail, "daemon", "daemon", daemonUnreachableDetail(url, err), d.serviceRestartCommand())
		return
	}

	expected := brwidentity.Identity{Workspace: d.req.Workspace, Profile: d.profile.Name}
	if mismatches := health.Identity.Mismatches(expected); len(mismatches) > 0 && !health.Identity.Empty() {
		d.add(checkFail, "daemon", "daemon",
			fmt.Sprintf("%s answers, but it serves %s", url, strings.Join(mismatches, ", ")),
			"brwctl daemons   # then re-run setup with a free --http-port")
		return
	}
	d.health = &health
	if want := d.req.Version; want != "" && want != "dev" && health.Version != want {
		running := "a build that does not report its version"
		if health.Version != "" {
			running = "build " + health.Version
		}
		d.add(checkFail, "daemon", "daemon",
			fmt.Sprintf("%s is up but runs %s; the installed build is %s", url, running, want),
			d.serviceRestartCommand())
		return
	}
	detail := url + " is up"
	if health.Version != "" {
		detail += " on build " + health.Version
	}
	if health.TabLeases.ActiveTabs > 0 {
		detail += fmt.Sprintf(", %d tab lease(s) held, %d request(s) in flight", health.TabLeases.ActiveTabs, health.TabLeases.InFlight)
	}
	d.add(checkOK, "daemon", "daemon", detail, "")
}

func daemonUnreachableDetail(url string, err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing is listening on " + url + "; the daemon is not running, or it was configured on another port"
	case isTimeout(err):
		return url + " accepted the connection but did not answer /health in time; the daemon is bound but wedged"
	case errors.Is(err, errUnexpectedResponse):
		return "something other than brwd is listening on " + url + ": " + err.Error()
	default:
		return url + " is unreachable: " + err.Error()
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (d *doctorRun) checkBridgeConnected() {
	if !d.resolved {
		d.add(checkSkip, "bridge_connected", "extension bridge", "no profile resolved", "")
		return
	}
	if !d.profile.ExtensionBridgeAllowed {
		d.add(checkSkip, "bridge_connected", "extension bridge", "direct-CDP profile: brw drives its own browser, no bridge", "")
		return
	}
	if d.req.SkipLiveChecks {
		d.add(checkSkip, "bridge_connected", "extension bridge", "live checks skipped; probe with: brwctl doctor"+d.workspaceFlag(), "")
		return
	}
	addr := d.result.BridgeWSAddr
	status, err := probeBridgeStatus(d.client, addr)
	if err != nil {
		d.add(checkFail, "bridge_connected", "extension bridge",
			daemonUnreachableDetail("http://"+addr+"/status", err), d.serviceRestartCommand())
		return
	}
	d.bridge = &status
	if !status.Connected {

		detail := addr + " is listening but no extension has connected"
		if status.DisconnectReason != "" {
			detail += " (last disconnect: " + status.DisconnectReason + ")"
		}
		fix := d.reloadExtensionCommand()
		if staleHandshakeToken(status.DisconnectReason) {
			fix = d.bridgeSettingsCommand(addr)
		}
		d.add(checkFail, "bridge_connected", "extension bridge", detail, fix)
		return
	}
	detail := fmt.Sprintf("connected since %s", status.ConnectedAt)
	if status.Hello.Build != "" {
		detail = "extension build " + status.Hello.Build + " " + detail
	}
	d.add(checkOK, "bridge_connected", "extension bridge", detail, "")
}

func staleHandshakeToken(reason string) bool {
	return strings.Contains(reason, "invalid handshake token")
}

func (d *doctorRun) checkExtensionVersion() {
	if !d.resolved {
		d.add(checkSkip, "extension_version", "extension version", "no profile resolved", "")
		return
	}
	if !d.profile.ExtensionBridgeAllowed {
		d.add(checkSkip, "extension_version", "extension version", "direct-CDP profile: no extension payload is loaded", "")
		return
	}
	payloadDir := filepath.Join(d.req.AppDir, "extension")
	expected, err := setup.ExtensionPayloadVersion(payloadDir)
	if err != nil {
		d.add(checkFail, "extension_version", "extension version",
			"cannot read the installed extension payload: "+err.Error(),
			"curl -fsSL "+installScriptURL+" | sh")
		return
	}
	d.result.ExtensionPayloadVersion = expected

	perProfile, err := setup.PerProfileExtensionDirs(d.req.AppDir)
	if err == nil {
		var stale []string
		for _, dir := range perProfile {
			version, err := setup.ExtensionPayloadVersion(dir)
			if err != nil || version != expected {
				stale = append(stale, filepath.Base(dir))
			}
		}
		if len(stale) > 0 {
			d.add(checkFail, "extension_version", "extension version",
				fmt.Sprintf("payload is %s but %s on disk %s behind", expected, strings.Join(stale, ", "), pluralIs(len(stale))),
				"brwctl upgrade --refresh-extensions")
			return
		}
	}

	if d.bridge == nil || !d.bridge.Connected {
		d.add(checkSkip, "extension_version", "extension version",
			"payload "+expected+" on disk; no connected extension to compare it against", "")
		return
	}
	loaded := d.bridge.Hello.Build
	d.result.ExtensionLoadedVersion = loaded
	if loaded != "" && loaded != expected {
		d.add(checkFail, "extension_version", "extension version",
			fmt.Sprintf("the browser is running build %s but the installed payload is %s", loaded, expected),
			d.reloadExtensionCommand())
		return
	}
	d.add(checkOK, "extension_version", "extension version", "payload and loaded build are both "+expected, "")
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func (d *doctorRun) checkMCPRegistration() {
	configPath := setup.ClaudeConfigPath(d.req.Home)
	expectedBRWD := brwdPath(d.req.AppDir, d.req.Executable, d.req.GOOS, d.req.Runner.look)
	spec, specErr := deriveMCPServer(d.policy, mcpConfigRequest{
		Workspace:  d.req.Workspace,
		Profile:    d.req.Profile,
		Transport:  setup.LocalTransportName,
		PolicyPath: d.result.ProfilePolicyPath,
		Mode:       "auto",
	})
	name := "brw"
	addCommand := "brwctl setup" + d.workspaceFlag() + " --mcp-client claude"
	if specErr == nil {
		name = spec.Name
		addCommand = setup.Command(claudeAddArgs(spec))
	}

	servers, found, err := setup.ReadMCPServers(configPath)
	if err != nil {
		d.add(checkFail, "mcp_registration", "MCP registration",
			"cannot read "+configPath+": "+err.Error(), addCommand)
		return
	}
	entry, ok := servers[name]
	if !ok {

		if d.codexRegisters(name) {
			d.add(checkOK, "mcp_registration", "MCP registration", name+" is registered with codex", "")
			return
		}
		absent := fmt.Sprintf("%s registers no MCP server named %q", configPath, name)
		if !found {
			absent = "no agent client config at " + configPath + "; nothing on this machine is configured to launch brw"
		}
		if d.policy.MCPClient == "none" {

			d.add(checkWarn, "mcp_registration", "MCP registration",
				absent+"; this machine was set up with --mcp-client none, so brw cannot see where it is registered", addCommand)
			return
		}
		_, claudeOnPath := d.req.Runner.look("claude")
		if _, codexOnPath := d.req.Runner.look("codex"); codexOnPath {
			absent += ", and the codex CLI has no brw server either"
		} else if !claudeOnPath {

			d.add(checkWarn, "mcp_registration", "MCP registration",
				absent+"; no agent client CLI is on PATH, so brw may be registered in one brw cannot see", addCommand)
			return
		}
		d.add(checkFail, "mcp_registration", "MCP registration", absent, addCommand)
		return
	}
	replace := "claude mcp remove -s user " + name + " && " + addCommand
	if _, err := os.Stat(entry.Command); err != nil {
		d.add(checkFail, "mcp_registration", "MCP registration",
			fmt.Sprintf("%q points at %s, which does not exist", name, entry.Command), replace)
		return
	}
	if !samePath(entry.Command, expectedBRWD) {
		d.add(checkFail, "mcp_registration", "MCP registration",
			fmt.Sprintf("%q launches %s, but this install's daemon is %s", name, entry.Command, expectedBRWD), replace)
		return
	}
	d.add(checkOK, "mcp_registration", "MCP registration", name+" launches "+entry.Command+" (from "+configPath+")", "")
}

func (d *doctorRun) codexRegisters(name string) bool {
	if _, onPath := d.req.Runner.look("codex"); !onPath {
		return false
	}
	_, err := d.req.Runner.run("codex", "mcp", "get", name)
	return err == nil
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

func (d *doctorRun) checkClaudeInChrome() {
	state := setup.DetectClaudeInChrome(setup.ClaudeConfigPath(d.req.Home))
	if !state.Enabled {
		d.add(checkOK, "claude_in_chrome", "Claude-in-Chrome conflict", "no competing Chrome integration is enabled", "")
		return
	}
	message := "Claude Code's own Chrome integration is enabled; run /chrome in Claude Code and turn it off, or the agent sees two browser tool sets and may drive the wrong browser"
	d.result.Warnings = append(d.result.Warnings, doctorWarning{
		Name:    setup.ClaudeInChromeWarning,
		Message: message,
		Detail:  strings.Join(state.Signals, ", ") + " in " + state.Path,
	})
	d.add(checkWarn, "claude_in_chrome", "Claude-in-Chrome conflict", message, "claude   # then run /chrome and turn it off")
}

func (d *doctorRun) checkTransport() {
	if !d.resolved {
		d.add(checkSkip, "transport", "transport capabilities", "no profile resolved", "")
		return
	}

	transport := ""
	if d.health != nil {
		transport = d.health.Identity.Transport
	}
	if transport == "" {
		transport = setup.ResolvedTransport(d.profile)
	}
	if transport == "" {
		d.add(checkFail, "transport", "transport capabilities",
			"profile "+d.profile.Name+" allows neither direct CDP nor the extension bridge, so no tool can run",
			"brwctl setup"+d.workspaceFlag())
		return
	}
	capabilities := setup.CapabilitiesFor(transport)
	d.result.Transport = transport
	d.result.Capabilities = &capabilities
	summary := transport + " — has " + capabilities.Has + "; lacks " + capabilities.Lacks
	if d.req.SkipLiveChecks {
		d.add(checkSkip, "transport", "transport capabilities",
			summary+"; live checks skipped, so nothing here says the lane is up", "")
		return
	}

	if blocker, dead := d.deadLane(transport); dead {
		d.add(checkFail, "transport", "transport capabilities",
			transport+" is configured but not live: "+blocker.Detail, blocker.Fix)
		return
	}
	d.add(checkOK, "transport", "transport capabilities", summary, "")
}

func (d *doctorRun) deadLane(transport string) (doctorCheck, bool) {
	names := []string{"daemon"}
	if transport == setup.ResolvedExtensionBridge {
		names = append(names, "bridge_connected")
	}
	for _, name := range names {
		if check, found := d.checkNamed(name); found && check.Status == checkFail {
			return check, true
		}
	}
	return doctorCheck{}, false
}

func (d *doctorRun) checkNamed(name string) (doctorCheck, bool) {
	for _, check := range d.result.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return doctorCheck{}, false
}

type daemonHealth struct {
	OK        bool                 `json:"ok"`
	Version   string               `json:"version"`
	Identity  brwidentity.Identity `json:"identity"`
	TabLeases tabLeaseStats        `json:"tab_leases"`
}

type tabLeaseStats struct {
	ActiveTabs int `json:"active_tabs"`
	Owners     int `json:"owners"`
	InFlight   int `json:"in_flight"`
}

type bridgeStatus struct {
	Connected bool `json:"connected"`
	Hello     struct {
		Build  string `json:"build"`
		Chrome string `json:"chrome"`
		Label  string `json:"label"`
		// StatusURL and ConfigSource are the endpoint the connected extension is using and the config layer that supplied it.
		StatusURL    string `json:"status_url"`
		ConfigSource string `json:"config_source"`
	} `json:"hello"`
	// LastHandshake is what a REFUSED hello reported.
	LastHandshake struct {
		StatusURL    string `json:"status_url"`
		BridgeURL    string `json:"bridge_url"`
		ConfigSource string `json:"config_source"`
		Reason       string `json:"reason"`
		At           string `json:"at"`
	} `json:"last_handshake"`
	Pending          int    `json:"pending"`
	Inflight         int    `json:"inflight"`
	Queued           int    `json:"queued"`
	ConnectedAt      string `json:"connected_at"`
	DisconnectReason string `json:"disconnect_reason"`
}

var errUnexpectedResponse = errors.New("unexpected response")

func probeDaemonHealth(client *http.Client, httpURL string) (daemonHealth, error) {
	var health daemonHealth
	if err := fetchJSON(client, strings.TrimRight(httpURL, "/")+"/health", &health); err != nil {
		return health, err
	}
	if !health.OK {
		return health, fmt.Errorf("%w: /health did not report ok", errUnexpectedResponse)
	}
	return health, nil
}

func probeBridgeStatus(client *http.Client, wsAddr string) (bridgeStatus, error) {
	var status bridgeStatus
	url := wsAddr
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	err := fetchJSON(client, strings.TrimRight(url, "/")+"/status", &status)
	return status, err
}

func doctorClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: refuseRedirects}
}

func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func withoutRedirects(client *http.Client) *http.Client {
	gated := http.Client{}
	if client != nil {
		gated = *client
	}
	gated.CheckRedirect = refuseRedirects
	return &gated
}

func fetchJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d from %s", errUnexpectedResponse, resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("%w: %s response exceeds 1 MiB", errUnexpectedResponse, url)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %s did not return JSON: %v", errUnexpectedResponse, url, err)
	}
	return nil
}

func renderDoctor(w io.Writer, report doctorResult) {
	header := "brw doctor"
	if report.Profile != "" {
		header += ": profile " + report.Profile
	}
	if report.Transport != "" {
		header += " on " + report.Transport
	}
	fmt.Fprintln(w, header)
	fmt.Fprintln(w)
	const titleWidth = 26
	for _, check := range report.Checks {
		fmt.Fprintf(w, "  %-5s %-*s %s\n", check.Status, titleWidth, check.Title, check.Detail)
		if check.Fix != "" && check.Status != checkOK {
			fmt.Fprintf(w, "  %-5s %-*s run: %s\n", "", titleWidth, "", check.Fix)
		}
	}
	fmt.Fprintln(w)
	if report.OK {
		fmt.Fprintln(w, "all checks passed")
		return
	}
	fmt.Fprintf(w, "%d check(s) failed\n", len(report.Failures))
}
