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
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// Check statuses. A check is red only when the machine cannot do what the check
// covers; skip is for a check that does not apply to this install (no bridge on
// a direct-CDP profile), and warn for something that works but will bite later.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

// doctorCheckNames is every check doctor can emit, in report order. It is the
// schema a consumer switches on, so a check is added here deliberately rather
// than appearing because some code path happened to run.
var doctorCheckNames = []string{
	"profile_policy",
	"profile_resolved",
	"app_files",
	"browser_binary",
	"browser_profile_dir",
	"bridge_extension",
	"daemon",
	"bridge_connected",
	"extension_version",
	"mcp_registration",
	"claude_in_chrome",
	"transport",
}

// doctorCheck is one diagnostic. Name is the stable handle a consumer matches
// on and Title is what a human reads. Fix is a command to paste, never a
// restatement of the problem: a red check that cannot name the next command has
// not finished diagnosing, it has only finished complaining.
type doctorCheck struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorWarning is a named, non-fatal finding. The name is the stable handle a
// consumer matches on; the message is what a human reads.
type doctorWarning struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// doctorResult is the `brwctl doctor` JSON contract. Fields are only ever added
// to it: an existing consumer keeps reading the keys it already knows.
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

// doctorRequest is what doctorReport needs. Policy is optional: `brwctl setup`
// passes the policy it has just merged in memory so --dry-run can verify a
// configuration that is not on disk yet.
type doctorRequest struct {
	Workspace  string
	Profile    string
	PolicyPath string
	AppDir     string
	Home       string
	GOOS       string
	Policy     *profilepolicy.Policy
	// Executable is the running brwctl. It is how doctor knows which brwd an
	// MCP registration ought to name, so a registration left behind by an
	// install that has since moved reads as stale instead of as fine.
	Executable string
	Runner     commandRunner
	Timeout    time.Duration
	// SkipLiveChecks leaves the daemon and the bridge unprobed. `brwctl setup`
	// sets it because loading the extension is the first thing setup tells the
	// operator to do by hand afterwards: probing a bridge nothing has connected
	// to yet would end every successful setup with a red check.
	SkipLiveChecks bool
	// ResolveError is why the caller could not name a workspace itself. It is
	// reported as the profile check rather than returned, so a machine whose
	// policy binds no workspace still gets the whole report.
	ResolveError error
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
		// A machine configured by `brwctl setup` has exactly one binding, and
		// making the operator retype its generated name is the kind of friction
		// that sends people back to hand-editing the policy.
		//
		// Failing to pick one is a diagnosis, not a usage error, and it goes
		// through the full report rather than a hand-built one: --json is a
		// contract, and the commonest broken machine is exactly the one that
		// lands here.
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
		Timeout:      timeout,
		ResolveError: resolveErr,
	})
	return reportDoctor(os.Stdout, report, asJSON)
}

// reportDoctor prints the report and returns the command's exit condition: any
// failing check is a non-zero exit, so a wrapper script can gate on it without
// parsing anything.
func reportDoctor(w io.Writer, report doctorResult, asJSON bool) error {
	if asJSON {
		writeJSON(w, report)
	} else {
		renderDoctor(w, report)
	}
	if report.OK {
		return nil
	}
	return fmt.Errorf("%d doctor check(s) failed; run the commands printed above", len(report.Failures))
}

// soleWorkspace names the only workspace binding in the policy. More than one
// is ambiguous and the caller has to say which; none means the policy predates
// workspace bindings, so the profile name is the only handle.
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

// doctorRun accumulates one report. Every check runs even when an earlier one
// failed — an operator fixing a broken machine wants the whole list, not the
// first problem and then silence.
type doctorRun struct {
	req      doctorRequest
	result   doctorResult
	policy   profilepolicy.Policy
	profile  profilepolicy.Profile
	resolved bool
	client   *http.Client
	// bridge is the live bridge status, carried from the bridge check to the
	// extension-version check: the loaded build is only knowable from a
	// connected extension.
	bridge *bridgeStatus
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
		client: &http.Client{Timeout: req.Timeout},
	}
	d.checkPolicy()
	d.checkAppFiles()
	d.checkBrowserBinary()
	d.checkProfileDir()
	d.checkBridgeExtension()
	d.checkDaemon()
	d.checkBridgeConnected()
	d.checkExtensionVersion()
	d.checkMCPRegistration()
	d.checkClaudeInChrome()
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

// workspaceFlag is the argument that re-runs a command against this profile.
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
	d.result.DaemonHTTPURL = defaultBridgeHTTPURL(profile)
	if profile.ExtensionBridgeAllowed {
		d.result.BridgeWSAddr = defaultBridgeWSAddr(profile)
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

	// The app-directory copy of the policy is what `task install-mac` syncs for
	// a remote push; it is not the policy this run loaded, and a machine set up
	// by `brwctl setup` legitimately has none. Report it, do not fail on it.
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
	// Match the daemon: an unconfigured bridge already trusts the published
	// extension id, so doctor must verify against the same id rather than
	// failing a policy that simply did not repeat it.
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

// loadUnpackedCommand opens the page the operator loads the unpacked extension
// from. There is no CLI that can install an unpacked extension into a running
// browser profile, so the closest thing to a next command is opening the page
// with the directory to select already named.
func (d *doctorRun) loadUnpackedCommand() string {
	payload := filepath.Join(d.req.AppDir, "extension")
	if d.req.GOOS == "darwin" {
		return fmt.Sprintf("open -a %q chrome://extensions   # Developer mode, Load unpacked, select %s",
			setup.BrowserDisplayName(d.profile.Kind), payload)
	}
	exe := d.result.BrowserExecutable
	if exe == "" {
		exe = d.profile.Kind
	}
	return fmt.Sprintf("%s chrome://extensions   # Developer mode, Load unpacked, select %s", exe, payload)
}

// reloadExtensionCommand opens the page whose Reload button makes a browser
// pick up an extension payload that has changed on disk.
func (d *doctorRun) reloadExtensionCommand() string {
	if d.req.GOOS == "darwin" {
		return fmt.Sprintf("open -a %q chrome://extensions   # click Reload under brw",
			setup.BrowserDisplayName(d.profile.Kind))
	}
	exe := d.result.BrowserExecutable
	if exe == "" {
		exe = d.profile.Kind
	}
	return exe + " chrome://extensions   # click Reload under brw"
}

func (d *doctorRun) serviceParams() setup.ServiceParams {
	return setup.ServiceParams{
		GOOS:      d.req.GOOS,
		Workspace: d.req.Workspace,
		Profile:   d.result.Profile,
		Home:      d.req.Home,
	}
}

// serviceRestartCommand is the one command that puts this profile's daemon
// back. A machine with no unit installed cannot be restarted into life, so it
// is sent to setup instead.
func (d *doctorRun) serviceRestartCommand() string {
	params := d.serviceParams()
	if _, err := os.Stat(params.UnitPath()); err != nil {
		return "brwctl setup" + d.workspaceFlag()
	}
	return serviceRestartCommand(d.req.GOOS, params)
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
	// A daemon answering on this port for another workspace is the failure the
	// port number alone cannot show: everything looks up, and every tool drives
	// somebody else's browser.
	expected := brwidentity.Identity{Workspace: d.req.Workspace, Profile: d.profile.Name}
	if mismatches := health.Identity.Mismatches(expected); len(mismatches) > 0 && !health.Identity.Empty() {
		d.add(checkFail, "daemon", "daemon",
			fmt.Sprintf("%s answers, but it serves %s", url, strings.Join(mismatches, ", ")),
			"brwctl daemons   # then re-run setup with a free --http-port")
		return
	}
	detail := url + " is up"
	if health.TabLeases.ActiveTabs > 0 {
		detail += fmt.Sprintf(", %d tab lease(s) held, %d request(s) in flight", health.TabLeases.ActiveTabs, health.TabLeases.InFlight)
	}
	d.add(checkOK, "daemon", "daemon", detail, "")
}

// daemonUnreachableDetail separates the three ways a loopback daemon fails to
// answer, because they take different fixes: nothing bound, something bound
// that is not brwd, and bound but wedged.
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
		// Installed is not connected: the extension's MV3 service worker has to
		// be running and have completed the handshake before any tool works.
		detail := addr + " is listening but no extension has connected"
		if status.DisconnectReason != "" {
			detail += " (last disconnect: " + status.DisconnectReason + ")"
		}
		fix := d.reloadExtensionCommand()
		if handshakeRejected(status.DisconnectReason) {
			// A reload re-presents the same token, so the reload command is a
			// loop here. The token lives in the profile's own
			// bridge-defaults.json, and setup is what writes it.
			fix = "brwctl setup" + d.workspaceFlag() + "   # rewrites this profile's " + setup.BridgeDefaultsFile + ", then reload the extension"
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

// handshakeRejected reports whether the bridge turned the extension away at the
// handshake rather than losing a connection it had accepted. The two take
// different fixes: a rejected handshake is a missing or stale token, which the
// browser will present again unchanged however often it is reloaded. The prefix
// is the one extensionbridge.recordHandshakeRejection writes; an older daemon
// reports nothing here and gets the reload command, as before.
func handshakeRejected(reason string) bool {
	return strings.HasPrefix(reason, "handshake rejected")
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

	// A per-profile copy that has fallen behind is executable code the daemon
	// has no view of: the browser keeps running it and nothing says so.
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
		// Claude Code's JSON config is not the only place a registration can
		// live: `brwctl setup --mcp-client codex` writes codex's TOML, which
		// nothing here can parse, so ask codex itself. Reporting a codex-only
		// machine red sends the operator to re-register a client they chose not
		// to use.
		if d.codexRegisters(name) {
			d.add(checkOK, "mcp_registration", "MCP registration", name+" is registered with codex", "")
			return
		}
		absent := fmt.Sprintf("%s registers no MCP server named %q", configPath, name)
		if !found {
			absent = "no agent client config at " + configPath + "; nothing on this machine is configured to launch brw"
		}
		_, claudeOnPath := d.req.Runner.look("claude")
		if _, codexOnPath := d.req.Runner.look("codex"); codexOnPath {
			absent += ", and the codex CLI has no brw server either"
		} else if !claudeOnPath {
			// With neither client installed there is nothing here to say brw is
			// unregistered: ~/.claude.json outlives the install that wrote it,
			// and setup's --mcp-client none hands the config to a client brw
			// has no way to read.
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

// codexRegisters asks the codex CLI whether it holds this server, the way
// setup's own registration step does. codex keeps its MCP servers in TOML and
// there is no TOML parser in this module, so the CLI is the only reader.
func (d *doctorRun) codexRegisters(name string) bool {
	if _, onPath := d.req.Runner.look("codex"); !onPath {
		return false
	}
	_, err := d.req.Runner.run("codex", "mcp", "get", name)
	return err == nil
}

// samePath compares two commands as the filesystem sees them, so a bin/
// symlink and its target are not reported as a stale registration.
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
	// Name the lane and its capability gap. Both transports are complete
	// browsers, but incognito, HttpOnly cookies and download routing exist on
	// one and Chrome tab groups on the other, and nothing else tells the user
	// which one their install chose.
	transport := setup.ResolvedTransport(d.profile)
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
	// The lane the policy allows is not the lane that is carrying anything. A
	// green capability list on a machine whose daemon or bridge is down reads
	// as "these tools work here" when no tool can run at all.
	if blocker, dead := d.deadLane(); dead {
		d.add(checkFail, "transport", "transport capabilities",
			transport+" is configured but not live: "+blocker.Detail, blocker.Fix)
		return
	}
	d.add(checkOK, "transport", "transport capabilities", summary, "")
}

// deadLane names the failed check that stops this profile's transport carrying
// a tool call. The bridge only counts on the lane that uses it.
func (d *doctorRun) deadLane() (doctorCheck, bool) {
	names := []string{"daemon"}
	if d.profile.ExtensionBridgeAllowed {
		names = append(names, "bridge_connected")
	}
	for _, name := range names {
		if check, found := d.checkNamed(name); found && check.Status == checkFail {
			return check, true
		}
	}
	return doctorCheck{}, false
}

// checkNamed returns a check this run has already reported.
func (d *doctorRun) checkNamed(name string) (doctorCheck, bool) {
	for _, check := range d.result.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return doctorCheck{}, false
}

// daemonHealth is the part of brwd's /health that doctor and upgrade read.
// TabLeases is what says whether an agent is mid-operation right now.
type daemonHealth struct {
	OK        bool                 `json:"ok"`
	Identity  brwidentity.Identity `json:"identity"`
	TabLeases tabLeaseStats        `json:"tab_leases"`
}

type tabLeaseStats struct {
	ActiveTabs int `json:"active_tabs"`
	Owners     int `json:"owners"`
	InFlight   int `json:"in_flight"`
}

// bridgeStatus is the part of the extension bridge's /status doctor and upgrade
// read. The endpoint also serves the handshake token to a loopback caller; it is
// deliberately not a field here, so the secret is never decoded, printed or put
// in a report.
type bridgeStatus struct {
	Connected bool `json:"connected"`
	Hello     struct {
		Build  string `json:"build"`
		Chrome string `json:"chrome"`
		Label  string `json:"label"`
	} `json:"hello"`
	Pending          int    `json:"pending"`
	Inflight         int    `json:"inflight"`
	Queued           int    `json:"queued"`
	ConnectedAt      string `json:"connected_at"`
	DisconnectReason string `json:"disconnect_reason"`
}

// errUnexpectedResponse marks an address that answered with something other
// than the document that was asked for. On a loopback daemon address that means
// a port collision rather than a dead daemon.
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

func fetchJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d from %s", errUnexpectedResponse, resp.StatusCode, url)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: %s did not return JSON: %v", errUnexpectedResponse, url, err)
	}
	return nil
}

// renderDoctor prints the report as a table whose fix column is the point: a
// red line the operator cannot act on is a red line they learn to ignore.
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
