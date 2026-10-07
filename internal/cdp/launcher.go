package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultShutdownGrace = 10 * time.Second

type LaunchConfig struct {
	ChromePath       string
	UserDataDir      string
	ProfileDirectory string
	Port             int
	Extensions       []string
	Args             []string
	// AllowRealProfile overrides the refusal to launch against the user's real browser profile (see EnsureSafeUserDataDir).
	AllowRealProfile bool
	// Network carries the launch-only network settings: proxy, certificate-error policy, and any privately trusted keys.
	Network NetworkEnvironment
	// Headless launches Chrome with --headless=new.
	Headless bool
}

type Launcher struct {
	cmd      *exec.Cmd
	endpoint string
	port     int
	grace    time.Duration
}

func Launch(ctx context.Context, cfg LaunchConfig) (*Launcher, error) {
	chromePath, err := FindChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	if cfg.UserDataDir == "" {
		cfg.UserDataDir = DefaultProfileDir("")
	}

	if err := EnsureSafeUserDataDir(effectiveUserDataDir(cfg.UserDataDir, cfg.Args), cfg.AllowRealProfile); err != nil {
		return nil, err
	}
	if err := cfg.Network.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.UserDataDir, 0o700); err != nil {
		return nil, err
	}
	port := cfg.Port
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, err
		}
	}

	args := launchArgs(cfg, port)

	cmd := exec.Command(chromePath, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return finishLaunch(ctx, cmd, port)
}

func launchArgs(cfg LaunchConfig, port int) []string {
	args := []string{
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + cfg.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",

		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	if cfg.Headless {

		args = append(args, "--headless=new", "--window-size=1440,900")
	}
	if cfg.ProfileDirectory != "" {
		args = append(args, "--profile-directory="+cfg.ProfileDirectory)
	}
	if len(cfg.Extensions) > 0 {
		args = append(args, "--load-extension="+strings.Join(cfg.Extensions, ","))
	}

	args = append(args, networkArgs(cfg.Network)...)
	args = append(args, cfg.Args...)
	args = append(args, "about:blank")
	return args
}

func finishLaunch(ctx context.Context, cmd *exec.Cmd, port int) (*Launcher, error) {
	launcher := &Launcher{cmd: cmd, endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), port: port, grace: defaultShutdownGrace}
	if err := launcher.waitReady(ctx, 15*time.Second); err != nil {
		_ = launcher.Close()
		return nil, err
	}
	return launcher, nil
}

func (l *Launcher) Endpoint() string {
	return l.endpoint
}

func (l *Launcher) Port() int {
	return l.port
}

func (l *Launcher) Close() error {
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	grace := l.grace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	done := make(chan error, 1)
	go func() { done <- l.cmd.Wait() }()

	if err := l.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = l.cmd.Process.Kill()
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = l.cmd.Process.Kill()
		return <-done
	}
}

func (l *Launcher) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := probe(deadline, l.endpoint); err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("chrome CDP endpoint did not become ready at %s: %w", l.endpoint, deadline.Err())
		case <-ticker.C:
		}
	}
}

func probe(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if payload.WebSocketDebuggerURL == "" {
		return fmt.Errorf("missing webSocketDebuggerUrl")
	}
	return nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// EnsureSafeUserDataDir refuses to launch Chrome in the two situations that corrupt the user's profile — losing live logins (e.g.
func EnsureSafeUserDataDir(userDataDir string, allowRealProfile bool) error {
	if strings.TrimSpace(userDataDir) == "" {
		return nil
	}
	if !allowRealProfile && isKnownBrowserProfileRoot(userDataDir) {
		return fmt.Errorf("refusing to launch Chrome against what looks like your real browser profile (%s): a second Chrome on a live profile corrupts it and logs you out of sites like WhatsApp Web. Use a dedicated --user-data-dir, the extension bridge, or --remote to attach; pass --unsafe-real-profile to override", userDataDir)
	}
	if runningChromeOwns(userDataDir) && !reclaimOrphanedChrome(userDataDir) {
		return fmt.Errorf("another Chrome is already running on %s (SingletonLock held by a live process); launching a second Chrome on the same profile can corrupt it — close that Chrome, or use --remote to attach to it instead", userDataDir)
	}
	return nil
}

func knownBrowserProfileRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	rel := []string{
		"Library/Application Support/Google/Chrome",
		"Library/Application Support/Google/Chrome Beta",
		"Library/Application Support/Google/Chrome Canary",
		"Library/Application Support/Chromium",
		"Library/Application Support/BraveSoftware/Brave-Browser",
		"Library/Application Support/Microsoft Edge",
		".config/google-chrome",
		".config/google-chrome-beta",
		".config/chromium",
		".config/microsoft-edge",
		".config/BraveSoftware/Brave-Browser",

		"AppData/Local/Google/Chrome/User Data",
		"AppData/Local/Google/Chrome Beta/User Data",
		"AppData/Local/Google/Chrome SxS/User Data",
		"AppData/Local/Chromium/User Data",
		"AppData/Local/Microsoft/Edge/User Data",
		"AppData/Local/BraveSoftware/Brave-Browser/User Data",
	}
	out := make([]string, 0, len(rel))
	for _, r := range rel {
		out = append(out, filepath.Clean(filepath.Join(home, r)))
	}
	return out
}

func effectiveUserDataDir(base string, args []string) string {
	dir := base
	for i := 0; i < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i], "--user-data-dir="); ok {
			dir = v
		} else if args[i] == "--user-data-dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	return dir
}

func isKnownBrowserProfileRoot(dir string) bool {
	for _, cand := range pathIdentities(dir) {
		for _, root := range knownBrowserProfileRoots() {
			for _, rootID := range pathIdentities(root) {

				if strings.EqualFold(cand, rootID) {
					return true
				}
			}
		}
	}
	return false
}

// IsInsideRealBrowserProfile reports whether dir is one of the user's real browser user-data-dirs or any path inside one.
func IsInsideRealBrowserProfile(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	candidates := append(pathIdentities(dir), resolvedThroughAncestor(filepath.Clean(dir)))
	for _, root := range knownBrowserProfileRoots() {
		for _, rootID := range pathIdentities(root) {
			for _, cand := range candidates {
				if pathIsWithin(cand, rootID) {
					return true
				}
			}
		}
	}
	return false
}

func pathIsWithin(path, root string) bool {
	p, r := strings.ToLower(path), strings.ToLower(root)
	return p == r || strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator))
}

func resolvedThroughAncestor(p string) string {
	var rest []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
	}
}

func pathIdentities(p string) []string {
	if strings.TrimSpace(p) == "" {
		return nil
	}
	clean := filepath.Clean(p)
	ids := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		ids = append(ids, resolved)
	}
	return ids
}

func runningChromeOwns(dir string) bool {
	pid, ok := singletonLockPID(dir)
	return ok && processAlive(pid)
}

func singletonLockPID(dir string) (int, bool) {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return 0, false
	}
	i := strings.LastIndex(target, "-")
	if i < 0 || i+1 >= len(target) {
		return 0, false
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func reclaimOrphanedChrome(dir string) bool {
	pid, ok := singletonLockPID(dir)
	if !ok {
		return false
	}
	out, err := exec.Command("ps", "-ww", "-o", "ppid=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	ppidText, cmdline, found := strings.Cut(line, " ")
	if !found {
		return false
	}
	ppid, err := strconv.Atoi(ppidText)
	if err != nil || !isOrphanedAutomationChrome(ppid, strings.TrimSpace(cmdline), dir) {
		return false
	}
	fmt.Fprintf(os.Stderr, "reclaiming %s: stopping orphaned Chrome pid %d left by a daemon that was killed\n", dir, pid)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		_ = proc.Signal(sig)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(pid) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return !processAlive(pid)
}

func isOrphanedAutomationChrome(ppid int, cmdline, dir string) bool {
	if ppid != 1 {
		return false
	}
	padded := " " + cmdline + " "
	if !strings.Contains(padded, " --remote-debugging-port=") && !strings.Contains(padded, " --remote-debugging-pipe ") {
		return false
	}
	want := filepath.Clean(dir)
	return strings.Contains(padded, " --user-data-dir="+want+" ") || strings.Contains(padded, " --user-data-dir "+want+" ")
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return proc.Signal(syscall.Signal(0)) == nil
}
