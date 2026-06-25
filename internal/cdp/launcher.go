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

// defaultShutdownGrace is how long Close waits for Chrome to quit gracefully
// after SIGTERM before escalating to SIGKILL. It must be generous: Chrome
// flushes its profile stores (LevelDB/IndexedDB/Preferences) on exit, and a
// hard kill mid-flush corrupts the profile (lost sessions, "profile won't
// open"). The old 2s window routinely fired mid-flush on a busy profile.
const defaultShutdownGrace = 10 * time.Second

type LaunchConfig struct {
	ChromePath       string
	UserDataDir      string
	ProfileDirectory string
	Port             int
	Extensions       []string
	Args             []string
	// AllowRealProfile overrides the refusal to launch against the user's real
	// browser profile (see EnsureSafeUserDataDir). Diagnostics only.
	AllowRealProfile bool
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
	// Refuse to corrupt a real/in-use profile BEFORE creating the dir or spawning
	// Chrome — this is the guard that prevents the "WhatsApp logged out + Chrome
	// won't reopen" failure when direct CDP is mistakenly pointed at the user's
	// live Chrome profile.
	// Validate the EFFECTIVE user-data-dir — the one Chrome will actually use
	// after the operator's passthrough args are applied — not just cfg.UserDataDir.
	// Chrome keeps the LAST --user-data-dir, and cfg.Args is appended after the
	// validated path below, so a --user-data-dir smuggled through cfg.Args would
	// otherwise silently override the checked dir and defeat this guard.
	if err := EnsureSafeUserDataDir(effectiveUserDataDir(cfg.UserDataDir, cfg.Args), cfg.AllowRealProfile); err != nil {
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

	args := []string{
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + cfg.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		// Keep in-page timers (setTimeout/setInterval) firing at their requested
		// rate. Chrome throttles timers to ~1Hz on hidden/occluded/headless
		// pages, which silently turned the 100ms actionability poll into a
		// ~700-900ms stall per click. These flags are standard for an automation
		// browser and only affect background-throttling, never foreground tabs.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	if cfg.ProfileDirectory != "" {
		args = append(args, "--profile-directory="+cfg.ProfileDirectory)
	}
	if len(cfg.Extensions) > 0 {
		args = append(args, "--load-extension="+strings.Join(cfg.Extensions, ","))
	}
	args = append(args, cfg.Args...)
	args = append(args, "about:blank")

	// Deliberately NOT exec.CommandContext(ctx, ...): binding Chrome's lifetime to
	// ctx means a cancelled ctx (the daemon's SIGTERM signal context on shutdown)
	// makes os/exec send its own SIGKILL, racing the graceful Close() below and
	// corrupting the profile on a normal Ctrl-C. Chrome is stopped solely by
	// Close(), which terminates it gracefully.
	cmd := exec.Command(chromePath, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

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
	// Ask Chrome to quit gracefully (SIGTERM) so it flushes profile state before
	// exiting; only escalate to SIGKILL if it ignores the request past the grace
	// window. Always drain Wait() after Kill so the process is reaped.
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
			return fmt.Errorf("Chrome CDP endpoint did not become ready at %s: %w", l.endpoint, deadline.Err())
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

// EnsureSafeUserDataDir refuses to launch Chrome in the two situations that
// corrupt the user's profile — losing live logins (e.g. WhatsApp Web must be
// relinked) and leaving Chrome unable to reopen:
//  1. the target dir is one of the user's REAL browser profiles (unless
//     allowRealProfile overrides), or
//  2. a live Chrome already owns the dir (its SingletonLock points at a running
//     pid); a second Chrome on the same profile contends over its LevelDB /
//     IndexedDB stores and corrupts them.
//
// An empty dir (caller hasn't resolved one yet) and a stale lock (dead pid) are
// both allowed.
func EnsureSafeUserDataDir(userDataDir string, allowRealProfile bool) error {
	if strings.TrimSpace(userDataDir) == "" {
		return nil
	}
	if !allowRealProfile && isKnownBrowserProfileRoot(userDataDir) {
		return fmt.Errorf("refusing to launch Chrome against what looks like your real browser profile (%s): a second Chrome on a live profile corrupts it and logs you out of sites like WhatsApp Web. Use a dedicated --user-data-dir, the extension bridge, or --remote to attach; pass --unsafe-real-profile to override", userDataDir)
	}
	if runningChromeOwns(userDataDir) {
		return fmt.Errorf("another Chrome is already running on %s (SingletonLock held by a live process); launching a second Chrome on the same profile can corrupt it — close that Chrome, or use --remote to attach to it instead", userDataDir)
	}
	return nil
}

// knownBrowserProfileRoots returns the user-data-dir roots of the user's REAL
// browsers — the profiles holding their live logins. brw must never CDP-launch a
// second Chrome against one of these.
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
		// Windows (%LOCALAPPDATA% is <home>\AppData\Local). brw builds
		// cross-platform, so the real-profile guard must cover Windows too.
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

// effectiveUserDataDir returns the --user-data-dir Chrome will actually use:
// the LAST occurrence among the base dir and the passthrough args (Chrome keeps
// the last value of a repeated switch). Both --user-data-dir=X and the
// space-separated --user-data-dir X forms are recognised.
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
				// EqualFold because macOS (APFS) and Windows are case-insensitive
				// by default, so a case-variant path opens the SAME profile.
				if strings.EqualFold(cand, rootID) {
					return true
				}
			}
		}
	}
	return false
}

// pathIdentities returns the cleaned path plus, when it exists, its
// symlink-resolved form, so a symlink pointing at the real profile cannot slip
// past an exact-string comparison.
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

// runningChromeOwns reports whether a LIVE Chrome currently owns dir, via the
// SingletonLock symlink Chrome maintains in every user-data-dir. The link target
// is "<host>-<pid>"; a live pid means a Chrome already holds the profile. A
// missing lock, a non-symlink, an unparseable target, or a dead pid (stale lock
// Chrome will clear itself) all return false.
func runningChromeOwns(dir string) bool {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return false
	}
	i := strings.LastIndex(target, "-")
	if i < 0 || i+1 >= len(target) {
		return false
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes for existence without affecting the process.
	return proc.Signal(syscall.Signal(0)) == nil
}
