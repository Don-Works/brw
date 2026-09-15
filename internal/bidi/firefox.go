package bidi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// firefoxShutdownGrace is how long Close waits after SIGTERM. Firefox flushes
// its profile stores on exit like Chrome does, and the prototype runs against
// throwaway profiles, so this is shorter than the launcher's 10s for Chrome.
const firefoxShutdownGrace = 5 * time.Second

// bidiEndpointLine is how Firefox announces the port it bound. There is no
// /json/version on a BiDi-only browser and no DevToolsActivePort file in the
// profile, so the startup line is the only discovery channel — which is why
// this prototype launches Firefox rather than attaching to one.
var bidiEndpointLine = regexp.MustCompile(`WebDriver BiDi listening on (ws://[^\s]+)`)

// FirefoxConfig is what LaunchFirefox needs.
type FirefoxConfig struct {
	// Path is the firefox executable. Empty searches the per-OS candidates.
	Path string
	// ProfileDir is a throwaway profile directory. Firefox writes into it, so it
	// must not be a profile the user signs in with.
	ProfileDir string
	// Headless runs with no visible window.
	Headless bool
	// Args are extra command-line arguments, appended last.
	Args []string
	// Prefs are written to the profile's user.js before launch. Firefox has no
	// BiDi command for the download directory (browsingContext.setDownloadBehavior
	// is an unknown command in 155), so a run that must not write into the
	// user's real Downloads folder has to say so here, at launch.
	Prefs map[string]any
	// ReadyTimeout bounds how long to wait for the endpoint line.
	ReadyTimeout time.Duration
}

// Firefox is a launched BiDi-speaking Firefox.
type Firefox struct {
	cmd      *exec.Cmd
	endpoint string
	logMu    sync.Mutex
	log      []string
}

// FindFirefox resolves the executable, mirroring cdp.FindChrome so the
// prototype fails the same recognisable way on a machine without the browser.
func FindFirefox(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	for _, candidate := range firefoxCandidates(runtime.GOOS) {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("firefox executable not found")
}

func firefoxCandidates(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Firefox.app/Contents/MacOS/firefox",
			"/Applications/Firefox Developer Edition.app/Contents/MacOS/firefox",
			"/Applications/Firefox Nightly.app/Contents/MacOS/firefox",
			"firefox",
		}
	default:
		return []string{"firefox", "firefox-esr", "firefox-developer-edition"}
	}
}

// LaunchFirefox starts Firefox with the remote agent on and returns once it has
// announced its BiDi endpoint.
//
// --remote-debugging-port 0 makes Firefox bind an ephemeral port and print it,
// which is the only way to run two prototypes at once without them fighting
// over a fixed port. --no-remote stops the launch attaching to an already
// running Firefox, which would drive the user's real windows.
func LaunchFirefox(ctx context.Context, cfg FirefoxConfig) (*Firefox, error) {
	path, err := FindFirefox(cfg.Path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.ProfileDir) == "" {
		return nil, errors.New("bidi: a throwaway profile directory is required")
	}
	if err := os.MkdirAll(cfg.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	if err := writeUserPrefs(cfg.ProfileDir, cfg.Prefs); err != nil {
		return nil, err
	}
	args := []string{"--no-remote", "--profile", cfg.ProfileDir, "--remote-debugging-port", "0"}
	if cfg.Headless {
		args = append(args, "--headless")
	}
	args = append(args, cfg.Args...)

	cmd := exec.Command(path, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ff := &Firefox{cmd: cmd}
	found := make(chan string, 1)
	go ff.scan(stderr, found)

	timeout := cfg.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	select {
	case endpoint := <-found:
		if endpoint == "" {
			_ = ff.Close()
			return nil, fmt.Errorf("firefox exited before announcing a BiDi endpoint: %s", ff.Log())
		}
		ff.endpoint = endpoint
		return ff, nil
	case <-time.After(timeout):
		_ = ff.Close()
		return nil, fmt.Errorf("firefox did not announce a BiDi endpoint within %s: %s", timeout, ff.Log())
	case <-ctx.Done():
		_ = ff.Close()
		return nil, ctx.Err()
	}
}

// writeUserPrefs renders prefs into the profile's user.js. Values are JSON
// encoded, which is the same literal syntax user_pref takes for the string,
// number and boolean types Firefox accepts.
func writeUserPrefs(profileDir string, prefs map[string]any) error {
	if len(prefs) == 0 {
		return nil
	}
	names := make([]string, 0, len(prefs))
	for name := range prefs {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		value, err := json.Marshal(prefs[name])
		if err != nil {
			return fmt.Errorf("encode pref %s: %w", name, err)
		}
		quoted, err := json.Marshal(name)
		if err != nil {
			return fmt.Errorf("encode pref name %s: %w", name, err)
		}
		fmt.Fprintf(&b, "user_pref(%s, %s);\n", quoted, value)
	}
	return os.WriteFile(filepath.Join(profileDir, "user.js"), []byte(b.String()), 0o600)
}

// scan reads Firefox's stderr, keeping it for error messages and reporting the
// endpoint line once. It drains to EOF so a full pipe never blocks the browser.
func (ff *Firefox) scan(r io.Reader, found chan<- string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	reported := false
	for scanner.Scan() {
		line := scanner.Text()
		ff.logMu.Lock()
		if len(ff.log) < 200 {
			ff.log = append(ff.log, line)
		}
		ff.logMu.Unlock()
		if reported {
			continue
		}
		if m := bidiEndpointLine.FindStringSubmatch(line); m != nil {
			reported = true
			found <- m[1]
		}
	}
	if !reported {
		found <- ""
	}
}

// Endpoint is the BiDi session URL to dial. Firefox announces the origin; the
// session lives at /session under it.
func (ff *Firefox) Endpoint() string {
	return strings.TrimSuffix(ff.endpoint, "/") + "/session"
}

// Log returns the captured stderr, for an error message that would otherwise
// say only that the browser did not start.
func (ff *Firefox) Log() string {
	ff.logMu.Lock()
	defer ff.logMu.Unlock()
	return strings.Join(ff.log, "\n")
}

// Close stops Firefox, escalating to SIGKILL only if it ignores SIGTERM.
func (ff *Firefox) Close() error {
	if ff == nil || ff.cmd == nil || ff.cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- ff.cmd.Wait() }()
	if err := ff.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = ff.cmd.Process.Kill()
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-time.After(firefoxShutdownGrace):
		_ = ff.cmd.Process.Kill()
		return <-done
	}
}
