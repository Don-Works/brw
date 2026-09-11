package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// Action statuses. They are the first column of every line setup prints, so a
// --dry-run transcript and a real transcript line up for a diff.
const (
	statusWould  = "would"
	statusDid    = "did"
	statusOK     = "ok"
	statusSkip   = "skip"
	statusRefuse = "refuse"
	statusWarn   = "warn"
	statusFail   = "fail"
)

const setupUsage = `usage: brwctl setup [options]

Take a machine that has brw binaries on it to a working, connected bridge:
write a profile policy, disable browser App Nap, install a per-user background
brwd service, register the MCP server with your agent CLI, install the brw
agent skill, and verify the result. Every step is idempotent and needs no sudo.

options:
  --profile NAME        profile name to create or reuse (default: derived from --browser and --transport)
  --workspace NAME      workspace binding name (default: derived from --browser and --transport)
  --browser NAME        chrome or chromium (default: whichever browser has been used on this machine)
  --profile-directory D browser profile directory inside the user data dir, for example "Profile 1"
                        (default: the first existing one, else Default)
  --transport NAME      bridge or direct-cdp (default: bridge). This is the runtime lane, not an
                        entry in the policy's "transports" array; setup writes a policy transport
                        named "local" either way.
  --mcp-client NAME     claude, codex, both, or none (default: claude)
  --http-port N         loopback control port; the bridge WebSocket uses N+1 (default: 17310)
  --profile-policy PATH profile policy JSON to create or merge into
  --app-dir PATH        brw app install directory
  --skills-dir PATH     skills/brw source directory to install from
  --no-service          skip the background service and print the foreground command instead
  --dry-run             print every action in order without performing any
  --yes                 do not prompt for confirmation
  --help                print this message`

type setupOptions struct {
	profileName      string
	workspace        string
	browser          string
	transport        string
	profileDirectory string
	mcpClient        string
	policyPath       string
	appDir           string
	skillsDir        string
	httpPort         int
	noService        bool
	dryRun           bool
	assumeYes        bool
	home             string
	goos             string
	executable       string
	workingDir       string
	out              io.Writer
	runner           commandRunner
}

// commandRunner is how setup reaches external tools (defaults, launchctl,
// claude). It is an interface so a test can assert the exact argv without a
// real subprocess, and so --dry-run can refuse every mutating call by
// construction rather than by remembering to branch.
type commandRunner interface {
	run(name string, args ...string) (string, error)
	look(name string) (string, bool)
}

type execRunner struct{}

func (execRunner) run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (execRunner) look(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

type setupAction struct {
	status string
	text   string
}

type setupStep struct {
	name    string
	actions []setupAction
}

type setupRunner struct {
	opts     setupOptions
	steps    []*setupStep
	current  *setupStep
	manual   []string
	failures []string
	// policy is the merged in-memory policy every later step derives from, so
	// --dry-run produces the same plan on a machine where nothing was written.
	policy       profilepolicy.Policy
	profile      profilepolicy.Profile
	resolvedPath string
	service      setup.ServiceParams
}

func setupCommand(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts setupOptions
	fs.StringVar(&opts.profileName, "profile", os.Getenv("BRW_PROFILE"), "profile name to create or reuse")
	fs.StringVar(&opts.workspace, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name")
	fs.StringVar(&opts.browser, "browser", "", "chrome or chromium")
	fs.StringVar(&opts.profileDirectory, "profile-directory", "", `browser profile directory inside the user data dir, for example "Profile 1"`)
	fs.StringVar(&opts.transport, "transport", setup.TransportBridge, "bridge or direct-cdp")
	fs.StringVar(&opts.mcpClient, "mcp-client", "claude", "claude, codex, both, or none")
	fs.StringVar(&opts.policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&opts.appDir, "app-dir", defaultAppDir(), "brw app install directory")
	fs.StringVar(&opts.skillsDir, "skills-dir", "", "skills/brw source directory")
	fs.IntVar(&opts.httpPort, "http-port", setup.DefaultHTTPPort, "loopback control port; the bridge WebSocket uses this plus one")
	fs.BoolVar(&opts.noService, "no-service", false, "skip the background service")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print every action without performing any")
	fs.BoolVar(&opts.assumeYes, "yes", false, "do not prompt for confirmation")
	if err := fs.Parse(args); err != nil {
		// A help request is a successful invocation, so an installer can probe
		// for this subcommand with `brwctl setup --help` and read the exit code.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, setupUsage)
			return nil
		}
		fmt.Fprintln(os.Stderr, setupUsage)
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, setupUsage)
		return fmt.Errorf("setup takes no positional arguments, got %q", fs.Arg(0))
	}

	opts.out = os.Stdout
	opts.goos = runtime.GOOS
	opts.runner = execRunner{}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	opts.home = home
	if executable, err := os.Executable(); err == nil {
		opts.executable = executable
	}
	if workingDir, err := os.Getwd(); err == nil {
		opts.workingDir = workingDir
	}
	if err := opts.normalise(); err != nil {
		return err
	}

	// A prompt has to show what it is asking about, so the confirmable run is a
	// dry run first and then the real one. Every probe in a plan is read-only,
	// so running it twice changes nothing.
	if !opts.dryRun && !opts.assumeYes && stdinIsTerminal() {
		preview := opts
		preview.dryRun = true
		if _, err := runSetup(preview); err != nil {
			return err
		}
		fmt.Fprint(opts.out, "\nProceed? [y/N] ")
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			fmt.Fprintln(opts.out, "aborted; nothing was changed")
			return nil
		}
		fmt.Fprintln(opts.out)
	}

	runner, err := runSetup(opts)
	if err != nil {
		return err
	}
	if len(runner.failures) > 0 {
		return fmt.Errorf("setup finished with %d failed step(s)", len(runner.failures))
	}
	return nil
}

func (o *setupOptions) normalise() error {
	switch o.transport {
	case setup.TransportBridge, setup.TransportDirectCDP:
	case "":
		o.transport = setup.TransportBridge
	default:
		return fmt.Errorf("--transport must be %s or %s", setup.TransportBridge, setup.TransportDirectCDP)
	}
	switch o.mcpClient {
	case "claude", "codex", "both", "none":
	case "":
		o.mcpClient = "claude"
	default:
		return errors.New("--mcp-client must be claude, codex, both, or none")
	}
	if o.browser == "" {
		o.browser = detectBrowser(o.goos, o.home)
	}
	if o.browser != setup.BrowserChrome && o.browser != setup.BrowserChromium {
		return fmt.Errorf("--browser must be %s or %s", setup.BrowserChrome, setup.BrowserChromium)
	}
	if o.httpPort <= 0 || o.httpPort > 65534 {
		return errors.New("--http-port must be between 1 and 65534; the bridge uses the next port up")
	}
	if o.profileName == "" {
		o.profileName = setup.DefaultProfileName(o.browser, o.transport)
	}
	if o.workspace == "" {
		o.workspace = setup.DefaultWorkspaceName(o.browser, o.transport)
	}
	return nil
}

// detectBrowser picks the browser to bridge. A browser that has actually been
// run wins over one that is merely installed: a bridge profile binds to a
// profile directory, and an installed-but-never-launched browser has none, so
// binding to it writes a policy that cannot verify. Chrome breaks the tie when
// both have been used. --browser overrides.
func detectBrowser(goos, home string) string {
	if setup.HasProfiles(browserDataDir(goos, home, setup.BrowserChrome)) {
		return setup.BrowserChrome
	}
	if setup.HasProfiles(browserDataDir(goos, home, setup.BrowserChromium)) {
		return setup.BrowserChromium
	}
	if goos == "darwin" {
		if _, err := os.Stat("/Applications/Google Chrome.app"); err == nil {
			return setup.BrowserChrome
		}
		if _, err := os.Stat("/Applications/Chromium.app"); err == nil {
			return setup.BrowserChromium
		}
		return setup.BrowserChrome
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable"} {
		if _, err := exec.LookPath(name); err == nil {
			return setup.BrowserChrome
		}
	}
	for _, name := range []string{"chromium", "chromium-browser"} {
		if _, err := exec.LookPath(name); err == nil {
			return setup.BrowserChromium
		}
	}
	return setup.BrowserChrome
}

// browserDataDir expands a policy-shaped user data directory against a known
// home, so detection works against a home that is not the process's own.
func browserDataDir(goos, home, browser string) string {
	path := setup.BrowserUserDataDir(goos, browser)
	if home != "" && strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return profilepolicy.ExpandPath(path)
}

func runSetup(opts setupOptions) (*setupRunner, error) {
	runner := &setupRunner{opts: opts}
	runner.header()
	if err := runner.stepConfig(); err != nil {
		return runner, err
	}
	runner.stepAppNap()
	runner.stepService()
	runner.stepMCPClient()
	runner.stepSkills()
	runner.stepVerify()
	runner.finish()
	return runner, nil
}

func (r *setupRunner) begin(name string) {
	step := &setupStep{name: name}
	r.steps = append(r.steps, step)
	r.current = step
	fmt.Fprintf(r.opts.out, "\n[%d/6] %s\n", len(r.steps), name)
}

func (r *setupRunner) act(status, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	r.current.actions = append(r.current.actions, setupAction{status: status, text: text})
	fmt.Fprintf(r.opts.out, "  %-7s %s\n", status, text)
	if status == statusFail {
		r.failures = append(r.failures, r.current.name+": "+text)
	}
}

// do performs one mutating action, or reports what it would have done. Every
// side effect in setup goes through here, so --dry-run cannot leak a write.
func (r *setupRunner) do(text string, perform func() error) bool {
	if r.opts.dryRun {
		r.act(statusWould, "%s", text)
		return true
	}
	if err := perform(); err != nil {
		r.act(statusFail, "%s: %v", text, err)
		return false
	}
	r.act(statusDid, "%s", text)
	return true
}

func (r *setupRunner) header() {
	brwd := r.brwdPath()
	fmt.Fprintln(r.opts.out, "brwctl setup")
	for _, row := range [][2]string{
		{"workspace", r.opts.workspace},
		{"profile", r.opts.profileName},
		{"browser", setup.BrowserDisplayName(r.opts.browser)},
		{"transport", r.opts.transport},
		{"brwd", brwd},
		{"app dir", r.opts.appDir},
	} {
		fmt.Fprintf(r.opts.out, "  %-11s %s\n", row[0], row[1])
	}
	if r.opts.dryRun {
		fmt.Fprintln(r.opts.out, "  mode        dry run; nothing will be changed")
	}
}

// brwdPath resolves the daemon an MCP client and the background service will
// launch. An absolute path is what makes brw start under a client that does not
// inherit a login shell's PATH; the app directory is checked first because that
// is where `make install-mac` and the macOS package put it, and it is not on
// PATH.
func (r *setupRunner) brwdPath() string {
	name := "brwd"
	if r.opts.goos == "windows" {
		name = "brwd.exe"
	}
	candidates := []string{filepath.Join(r.opts.appDir, "bin", name)}
	if r.opts.executable != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(r.opts.executable), name))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	if path, ok := r.opts.runner.look("brwd"); ok {
		return path
	}
	return "brwd"
}

func (r *setupRunner) stepConfig() error {
	r.begin("profile policy")
	path := r.opts.policyPath
	if path == "" {
		resolved, err := setup.DefaultPolicyPath(r.opts.home)
		if err != nil {
			return err
		}
		path = resolved
	}
	r.resolvedPath = path

	existing, found, err := setup.LoadPolicyFile(path)
	if err != nil {
		return err
	}
	if found {
		r.act(statusOK, "read existing policy %s", path)
	} else {
		r.act(statusOK, "no policy at %s; writing a complete default", path)
	}

	profileDirectory := r.opts.profileDirectory
	if profileDirectory == "" && r.opts.transport == setup.TransportBridge {
		dataDir := browserDataDir(r.opts.goos, r.opts.home, r.opts.browser)
		profileDirectory = setup.PickProfileDirectory(dataDir)
		if dirs := setup.ProfileDirectories(dataDir); len(dirs) > 0 {
			r.act(statusOK, "%s profile directories in %s: %s", setup.BrowserDisplayName(r.opts.browser), dataDir, strings.Join(dirs, ", "))
		} else {
			r.act(statusWarn, "%s has no profile directory under %s yet; binding to %q, the one it creates on first launch", setup.BrowserDisplayName(r.opts.browser), dataDir, profileDirectory)
		}
	}
	merged, changes := setup.Merge(existing, setup.PolicyRequest{
		Workspace:        r.opts.workspace,
		Profile:          r.opts.profileName,
		Browser:          r.opts.browser,
		Transport:        r.opts.transport,
		ProfileDirectory: profileDirectory,
		BRWDPath:         r.brwdPath(),
		HTTPPort:         r.opts.httpPort,
		Home:             r.opts.home,
		GOOS:             r.opts.goos,
	})
	for _, change := range changes {
		if !change.Edit {
			r.act(statusOK, "%s", change.Detail)
			continue
		}
		r.act(statusWould, "%s", change.Detail)
	}
	r.policy = merged
	profile, err := merged.Find(r.opts.profileName)
	if err != nil {
		return err
	}
	r.profile = profile

	if !setup.Changed(changes) {
		r.act(statusOK, "%s is already complete; leaving it untouched", path)
		return nil
	}
	note := ""
	if found {
		note = ", backing up the current file first"
	}
	var backup string
	written := r.do(fmt.Sprintf("write %s (0600)%s", path, note), func() error {
		saved, err := setup.WritePolicy(path, merged, time.Now())
		backup = saved
		return err
	})
	if written && backup != "" {
		r.act(statusOK, "previous policy saved as %s", backup)
	}
	return nil
}

func (r *setupRunner) stepAppNap() {
	r.begin("browser App Nap")
	if r.opts.goos != "darwin" {
		r.act(statusSkip, "App Nap is macOS only; nothing to do on %s", r.opts.goos)
		return
	}
	for _, bundleID := range setup.BrowserBundleIDs(r.opts.browser) {
		current, _ := r.opts.runner.run("defaults", "read", bundleID, "NSAppSleepDisabled")
		if strings.TrimSpace(current) == "1" {
			r.act(statusOK, "%s already has NSAppSleepDisabled=YES", bundleID)
			continue
		}
		r.do(fmt.Sprintf("defaults write %s NSAppSleepDisabled -bool YES", bundleID), func() error {
			_, err := r.opts.runner.run("defaults", "write", bundleID, "NSAppSleepDisabled", "-bool", "YES")
			return err
		})
	}
	r.act(statusOK, "App Nap applies at the next launch of %s; quit it fully and reopen", setup.BrowserDisplayName(r.opts.browser))
	r.manual = append(r.manual, fmt.Sprintf("Quit %s completely (Cmd-Q, not just closing the window) and reopen it, so the App Nap default takes effect.", setup.BrowserDisplayName(r.opts.browser)))
}

func (r *setupRunner) serviceParams() setup.ServiceParams {
	params := setup.ServiceParams{
		GOOS:       r.opts.goos,
		Workspace:  r.opts.workspace,
		Profile:    r.opts.profileName,
		PolicyPath: r.resolvedPath,
		BRWDPath:   r.brwdPath(),
		HTTPAddr:   defaultBridgeHostPort(r.profile, r.opts.httpPort),
		LogPath:    setup.DefaultLogPath(r.opts.goos, r.opts.home, r.opts.profileName),
		WorkingDir: r.opts.appDir,
		Home:       r.opts.home,
	}
	if r.profile.ExtensionBridgeAllowed {
		params.BridgeAddr = defaultBridgeWSAddr(r.profile)
	}
	return params
}

func (r *setupRunner) stepService() {
	r.begin("background service")
	r.service = r.serviceParams()
	if r.opts.noService {
		r.act(statusSkip, "--no-service; run the daemon in the foreground with:")
		r.act(statusSkip, "  %s", setup.Command(r.service.Args()))
		return
	}
	r.act(statusOK, "daemon binds http %s%s", r.service.HTTPAddr, bridgeNote(r.service.BridgeAddr))
	switch r.opts.goos {
	case "darwin":
		r.serviceLaunchd()
	case "windows":
		r.serviceWindows()
	default:
		r.serviceSystemd()
	}
}

func (r *setupRunner) serviceLaunchd() {
	unitDir := filepath.Join(r.opts.home, "Library", "LaunchAgents")
	units, err := setup.ScanServiceUnits(unitDir, r.opts.goos)
	if err != nil {
		r.act(statusFail, "read %s: %v", unitDir, err)
		return
	}
	if conflicts := setup.Conflicts(units, r.service); len(conflicts) > 0 {
		for _, conflict := range conflicts {
			r.act(statusRefuse, "%s already exists (label %s) and %s", conflict.Path, conflict.Label, conflict.Reason)
		}
		r.act(statusRefuse, "leaving the existing LaunchAgent alone; setup will not write %s", r.service.UnitPath())
		r.manual = append(r.manual, fmt.Sprintf("A LaunchAgent for this profile already exists and setup did not touch it. Keep it, or unload and delete it (launchctl bootout gui/%d/<label>) and re-run setup.", os.Getuid()))
		return
	}
	r.act(statusOK, "scanned %d existing brwd LaunchAgent(s) in %s; none claims this profile or these ports", len(units), unitDir)

	plist := setup.LaunchAgentPlist(r.service)
	path := r.service.UnitPath()
	existing, readErr := os.ReadFile(path)
	unchanged := readErr == nil && string(existing) == plist
	if unchanged {
		r.act(statusOK, "%s is already current", path)
	} else {
		if !r.do(fmt.Sprintf("write %s", path), func() error {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(plist), 0o644)
		}) {
			return
		}
	}
	r.do(fmt.Sprintf("mkdir -p %s", filepath.Dir(r.service.LogPath)), func() error {
		return os.MkdirAll(filepath.Dir(r.service.LogPath), 0o755)
	})

	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), r.service.Label())
	_, printErr := r.opts.runner.run("launchctl", "print", target)
	loaded := printErr == nil
	if loaded && unchanged {
		r.act(statusOK, "%s is already loaded", r.service.Label())
		r.act(statusOK, "logs at %s", r.service.LogPath)
		return
	}
	if loaded {
		r.do(fmt.Sprintf("launchctl bootout %s", target), func() error {
			_, err := r.opts.runner.run("launchctl", "bootout", target)
			return err
		})
	}
	r.do(fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), path), func() error {
		_, err := r.opts.runner.run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), path)
		return err
	})
	r.do(fmt.Sprintf("launchctl kickstart -k %s", target), func() error {
		_, err := r.opts.runner.run("launchctl", "kickstart", "-k", target)
		return err
	})
	r.act(statusOK, "logs at %s", r.service.LogPath)
}

func (r *setupRunner) serviceSystemd() {
	unitDir := filepath.Join(r.opts.home, ".config", "systemd", "user")
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if _, ok := r.opts.runner.look("systemctl"); !ok || !setup.SystemdAvailable(runtimeDir) {
		r.act(statusSkip, "no `systemd --user` instance for this login (XDG_RUNTIME_DIR=%q)", runtimeDir)
		r.act(statusSkip, "run brwd from your desktop session autostart or a shell instead:")
		r.act(statusSkip, "  nohup %s >> %s 2>&1 &", setup.Command(r.service.Args()), r.service.LogPath)
		r.manual = append(r.manual, "No user systemd on this machine: start brwd yourself, or add the printed nohup line to ~/.config/autostart or your shell profile.")
		return
	}
	units, err := setup.ScanServiceUnits(unitDir, r.opts.goos)
	if err != nil {
		r.act(statusFail, "read %s: %v", unitDir, err)
		return
	}
	if conflicts := setup.Conflicts(units, r.service); len(conflicts) > 0 {
		for _, conflict := range conflicts {
			r.act(statusRefuse, "%s already exists (unit %s) and %s", conflict.Path, conflict.Label, conflict.Reason)
		}
		r.act(statusRefuse, "leaving the existing unit alone; setup will not write %s", r.service.UnitPath())
		r.manual = append(r.manual, "A systemd --user unit for this profile already exists and setup did not touch it. Keep it, or disable and delete it and re-run setup.")
		return
	}

	unit := setup.SystemdUnit(r.service)
	path := r.service.UnitPath()
	existing, readErr := os.ReadFile(path)
	unchanged := readErr == nil && string(existing) == unit
	if unchanged {
		r.act(statusOK, "%s is already current", path)
	} else if !r.do(fmt.Sprintf("write %s", path), func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(unit), 0o644)
	}) {
		return
	}
	r.do(fmt.Sprintf("mkdir -p %s", filepath.Dir(r.service.LogPath)), func() error {
		return os.MkdirAll(filepath.Dir(r.service.LogPath), 0o755)
	})
	r.do("systemctl --user daemon-reload", func() error {
		_, err := r.opts.runner.run("systemctl", "--user", "daemon-reload")
		return err
	})
	r.do(fmt.Sprintf("systemctl --user enable --now %s.service", r.service.Label()), func() error {
		_, err := r.opts.runner.run("systemctl", "--user", "enable", "--now", r.service.Label()+".service")
		return err
	})
	r.act(statusOK, "logs at %s (and the user journal)", r.service.LogPath)
}

func (r *setupRunner) serviceWindows() {
	path := r.service.UnitPath()
	script := setup.WindowsLauncherScript(r.service)
	existing, readErr := os.ReadFile(path)
	if readErr == nil && string(existing) == script {
		r.act(statusOK, "%s is already current", path)
	} else if !r.do(fmt.Sprintf("write %s", path), func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(script), 0o644)
	}) {
		return
	}
	if _, err := r.opts.runner.run("schtasks", "/Query", "/TN", r.service.Label()); err == nil {
		r.act(statusOK, "scheduled task %s already exists", r.service.Label())
		r.act(statusOK, "logs at %s", r.service.LogPath)
		return
	}
	taskArgs := setup.WindowsTaskArgs(r.service)
	r.do(strings.Join(taskArgs, " "), func() error {
		_, err := r.opts.runner.run(taskArgs[0], taskArgs[1:]...)
		return err
	})
	r.act(statusOK, "logs at %s", r.service.LogPath)
}

func (r *setupRunner) stepMCPClient() {
	r.begin("MCP client registration")
	spec, err := deriveMCPServer(r.policy, mcpConfigRequest{
		Workspace:  r.opts.workspace,
		Profile:    r.opts.profileName,
		Transport:  setup.LocalTransportName,
		PolicyPath: r.resolvedPath,
		Mode:       "auto",
	})
	if err != nil {
		r.act(statusFail, "derive MCP server config: %v", err)
		return
	}
	r.act(statusOK, "server %q runs %s", spec.Name, setup.Command(append([]string{spec.Command}, spec.Args...)))

	if r.opts.mcpClient == "none" {
		r.act(statusSkip, "--mcp-client none; add this to your client's MCP config:")
		for _, line := range strings.Split(strings.TrimRight(spec.configJSON(), "\n"), "\n") {
			r.act(statusSkip, "  %s", line)
		}
		return
	}
	if r.opts.mcpClient == "claude" || r.opts.mcpClient == "both" {
		r.registerClaude(spec)
	}
	if r.opts.mcpClient == "codex" || r.opts.mcpClient == "both" {
		r.registerCodex(spec)
	}
}

// registerClaude goes through the claude CLI rather than editing ~/.claude.json.
// Claude Code rewrites that file while it is running, so a direct edit races it
// and can be lost or can clobber unrelated state.
func (r *setupRunner) registerClaude(spec mcpServerSpec) {
	if _, ok := r.opts.runner.look("claude"); !ok {
		r.act(statusSkip, "claude CLI not on PATH; register manually with:")
		r.act(statusSkip, "  %s", setup.Command(claudeAddArgs(spec)))
		r.manual = append(r.manual, "Register brw with your MCP client: "+setup.Command(claudeAddArgs(spec)))
		return
	}
	if _, err := r.opts.runner.run("claude", "mcp", "get", spec.Name); err == nil {
		r.act(statusOK, "claude already has an MCP server named %q; leaving it as it is", spec.Name)
		r.act(statusOK, "to replace it: claude mcp remove -s user %s && %s", spec.Name, setup.Command(claudeAddArgs(spec)))
		return
	}
	args := claudeAddArgs(spec)
	r.do(setup.Command(args), func() error {
		out, err := r.opts.runner.run(args[0], args[1:]...)
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
	r.manual = append(r.manual, "Restart Claude Code (or run /mcp) so it picks up the new brw MCP server.")
}

func (r *setupRunner) registerCodex(spec mcpServerSpec) {
	if _, ok := r.opts.runner.look("codex"); !ok {
		r.act(statusSkip, "codex CLI not on PATH; register manually with:")
		r.act(statusSkip, "  %s", setup.Command(codexAddArgs(spec)))
		return
	}
	if _, err := r.opts.runner.run("codex", "mcp", "get", spec.Name); err == nil {
		r.act(statusOK, "codex already has an MCP server named %q; leaving it as it is", spec.Name)
		return
	}
	args := codexAddArgs(spec)
	r.do(setup.Command(args), func() error {
		out, err := r.opts.runner.run(args[0], args[1:]...)
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
}

func claudeAddArgs(spec mcpServerSpec) []string {
	args := []string{"claude", "mcp", "add", "-s", "user", spec.Name}
	for _, key := range sortedEnvKeys(spec.Env) {
		args = append(args, "-e", key+"="+spec.Env[key])
	}
	args = append(args, "--", spec.Command)
	return append(args, spec.Args...)
}

func codexAddArgs(spec mcpServerSpec) []string {
	args := []string{"codex", "mcp", "add", spec.Name}
	for _, key := range sortedEnvKeys(spec.Env) {
		args = append(args, "--env", key+"="+spec.Env[key])
	}
	args = append(args, "--", spec.Command)
	return append(args, spec.Args...)
}

func (r *setupRunner) stepSkills() {
	r.begin("agent skill")
	source := r.opts.skillsDir
	if source == "" {
		found, err := setup.FindSkillSource(r.opts.appDir, r.opts.executable, r.opts.workingDir)
		if err != nil {
			r.act(statusSkip, "%v", err)
			r.manual = append(r.manual, "Install the brw agent skill by copying skills/brw into ~/.claude/skills/brw.")
			return
		}
		source = found
	}
	r.act(statusOK, "source %s", source)
	for _, destination := range setup.SkillDestinations(r.opts.home) {
		if r.opts.dryRun {
			r.act(statusWould, "install skill into %s", destination)
			continue
		}
		changed, err := setup.CopyTree(source, destination)
		switch {
		case err != nil:
			r.act(statusFail, "install skill into %s: %v", destination, err)
		case changed:
			r.act(statusDid, "installed skill into %s", destination)
		default:
			r.act(statusOK, "%s is already current", destination)
		}
	}
}

func (r *setupRunner) stepVerify() {
	r.begin("verify")
	report, err := doctorReport(doctorRequest{
		Workspace:  r.opts.workspace,
		Profile:    r.opts.profileName,
		PolicyPath: r.resolvedPath,
		AppDir:     r.opts.appDir,
		Home:       r.opts.home,
		Policy:     &r.policy,
	})
	if err != nil {
		r.act(statusFail, "doctor: %v", err)
		return
	}
	r.act(statusOK, "transport %s", report.Transport)
	r.act(statusOK, "has: %s", report.Capabilities.Has)
	r.act(statusOK, "lacks: %s", report.Capabilities.Lacks)
	for _, warning := range report.Warnings {
		r.act(statusWarn, "%s", warning.Message)
	}
	if report.OK {
		r.act(statusOK, "doctor reports no failures")
		return
	}
	for _, failure := range report.Failures {
		r.act(statusWarn, "doctor: %s", failure)
	}
}

func (r *setupRunner) finish() {
	if r.profile.ExtensionBridgeAllowed {
		extensionDir := filepath.Join(r.opts.appDir, "extension")
		if _, err := os.Stat(filepath.Join(extensionDir, "manifest.json")); err != nil && r.opts.workingDir != "" {
			if _, err := os.Stat(filepath.Join(r.opts.workingDir, "extension", "manifest.json")); err == nil {
				extensionDir = filepath.Join(r.opts.workingDir, "extension")
			}
		}
		browser := setup.BrowserDisplayName(r.opts.browser)
		r.manual = append([]string{
			fmt.Sprintf("Load the brw extension in %s: open chrome://extensions, turn on \"Developer mode\" (top right), click \"Load unpacked\", and select %s. The id must read %s.", browser, extensionDir, profilepolicy.DefaultBridgeExtensionID),
		}, r.manual...)
	}
	if state := setup.DetectClaudeInChrome(setup.ClaudeConfigPath(r.opts.home)); state.Enabled {
		r.manual = append(r.manual, "Claude Code's own Chrome integration is enabled. Run /chrome in Claude Code and turn it off, or the agent sees two browser tool sets and may drive the wrong browser.")
	}
	r.manual = append(r.manual, fmt.Sprintf("Confirm with: brwctl doctor --workspace %s", r.opts.workspace))

	fmt.Fprintln(r.opts.out, "\nstill to do by hand:")
	for i, item := range r.manual {
		fmt.Fprintf(r.opts.out, "  %d. %s\n", i+1, item)
	}
	if r.opts.dryRun {
		fmt.Fprintln(r.opts.out, "\ndry run: nothing above was performed")
	}
}

func bridgeNote(addr string) string {
	if addr == "" {
		return ""
	}
	return ", extension bridge ws " + addr
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// defaultBridgeHostPort is the daemon's control address for a profile, falling
// back to the port setup was asked for when the profile names none.
func defaultBridgeHostPort(profile profilepolicy.Profile, port int) string {
	addr := strings.TrimSpace(profile.BridgeHTTPAddr)
	if addr == "" {
		return "127.0.0.1:" + strconv.Itoa(port)
	}
	return strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
