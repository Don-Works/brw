package cdp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
)

func TestLaunchSendsInheritedProcessOutputToFile(t *testing.T) {
	root := t.TempDir()
	output, err := os.OpenFile(filepath.Join(root, "chrome.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	chrome := filepath.Join(root, "fixture-chrome")
	if err := os.WriteFile(chrome, []byte("#!/bin/sh\nprintf 'fixture stdout\\n'\nprintf 'fixture stderr\\n' >&2\n(sleep 0.1; printf 'fixture descendant\\n' >&2) &\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1/fixture"}`)
	}))
	defer server.Close()
	profile := browsertest.NewProfile(t)
	launcher, err := Launch(context.Background(), LaunchConfig{ChromePath: chrome, UserDataDir: profile.Dir(), Port: server.Listener.Addr().(*net.TCPAddr).Port, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	profile.StopWith(func() { _ = launcher.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "fixture stdout\n") && strings.Contains(string(data), "fixture stderr\n") && strings.Contains(string(data), "fixture descendant\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process output did not reach private file: %q", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := output.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private output permissions: info=%v err=%v", info, err)
	}
}

func TestLauncherCloseQuitsGracefullyBeforeKill(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &Launcher{cmd: cmd, grace: 5 * time.Second}

	start := time.Now()
	_ = l.Close()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Close took %v on a SIGTERM-responsive process; it should exit promptly, well under the %v grace", elapsed, l.grace)
	}
}

func TestLauncherCloseKillsAfterGrace(t *testing.T) {

	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skipf("perl not available to model a SIGTERM-ignoring process: %v", err)
	}
	cmd := exec.Command(perl, "-e", `$SIG{TERM}="IGNORE"; sleep 30;`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	grace := 300 * time.Millisecond
	l := &Launcher{cmd: cmd, grace: grace}

	start := time.Now()
	_ = l.Close()
	elapsed := time.Since(start)

	if elapsed < grace {
		t.Fatalf("Close returned after %v, before the %v grace — it must give the process time to quit gracefully first", elapsed, grace)
	}
	if elapsed > grace+3*time.Second {
		t.Fatalf("Close took %v; SIGKILL escalation should fire shortly after the %v grace", elapsed, grace)
	}
}

func TestLauncherCloseNilSafe(t *testing.T) {
	var l *Launcher
	if err := l.Close(); err != nil {
		t.Fatalf("Close on nil launcher = %v, want nil", err)
	}
	if err := (&Launcher{}).Close(); err != nil {
		t.Fatalf("Close on launcher with no process = %v, want nil", err)
	}
}

func TestEnsureSafeUserDataDirAllowsCleanDir(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureSafeUserDataDir(dir, false); err != nil {
		t.Fatalf("clean temp dir rejected: %v", err)
	}
	if err := EnsureSafeUserDataDir("", false); err != nil {
		t.Fatalf("empty dir should be allowed (caller resolves later): %v", err)
	}
}

func TestEffectiveUserDataDirHonorsArgsOverride(t *testing.T) {
	if got := effectiveUserDataDir("/safe/dir", nil); got != "/safe/dir" {
		t.Fatalf("no args: got %q, want /safe/dir", got)
	}
	if got := effectiveUserDataDir("/safe/dir", []string{"--user-data-dir=/real/profile"}); got != "/real/profile" {
		t.Fatalf("=form override: got %q, want /real/profile", got)
	}
	if got := effectiveUserDataDir("/safe/dir", []string{"--user-data-dir", "/real/profile"}); got != "/real/profile" {
		t.Fatalf("space-form override: got %q, want /real/profile", got)
	}
	if got := effectiveUserDataDir("/safe/dir", []string{"--user-data-dir=/a", "--user-data-dir=/b"}); got != "/b" {
		t.Fatalf("last-wins: got %q, want /b", got)
	}
}

func TestKnownBrowserProfileRootIsCaseInsensitive(t *testing.T) {
	roots := knownBrowserProfileRoots()
	if len(roots) == 0 {
		t.Skip("no known roots on this platform")
	}
	if !isKnownBrowserProfileRoot(strings.ToUpper(roots[0])) {
		t.Errorf("case-variant of a known profile root should be recognised: %q", roots[0])
	}
	if isKnownBrowserProfileRoot(filepath.Join(t.TempDir(), "totally-unrelated")) {
		t.Error("an unrelated dir must not be flagged as a browser profile root")
	}
}

func TestPathIdentitiesResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "decoy")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	ids := pathIdentities(link)
	resolvedReal, _ := filepath.EvalSymlinks(real)
	found := false
	for _, id := range ids {
		if id == resolvedReal || id == real {
			found = true
		}
	}
	if !found {
		t.Fatalf("pathIdentities(%q) = %v, expected to include resolved target %q", link, ids, resolvedReal)
	}
}

func TestEnsureSafeUserDataDirRefusesLiveSingletonLock(t *testing.T) {
	dir := t.TempDir()

	target := fmt.Sprintf("somehost-%d", os.Getpid())
	if err := os.Symlink(target, filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := EnsureSafeUserDataDir(dir, false)
	if err == nil {
		t.Fatal("expected refusal when a live Chrome owns the profile (SingletonLock -> live pid)")
	}

	if err := EnsureSafeUserDataDir(dir, true); err == nil {
		t.Fatal("live SingletonLock must be refused even with allowRealProfile=true")
	}
}

func TestEnsureSafeUserDataDirAllowsStaleSingletonLock(t *testing.T) {
	dir := t.TempDir()

	c := exec.Command("true")
	if err := c.Run(); err != nil {
		c = exec.Command("/usr/bin/true")
		if err := c.Run(); err != nil {
			t.Skipf("cannot spawn a throwaway process to obtain a dead pid: %v", err)
		}
	}
	deadPID := c.Process.Pid
	target := fmt.Sprintf("somehost-%d", deadPID)
	if err := os.Symlink(target, filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := EnsureSafeUserDataDir(dir, false); err != nil {
		t.Fatalf("stale SingletonLock (dead pid %d) must not block launch: %v", deadPID, err)
	}
}

func TestEnsureSafeUserDataDirRefusesRealProfileRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	realRoot := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	if !isKnownBrowserProfileRoot(realRoot) {
		t.Fatalf("isKnownBrowserProfileRoot(%q) = false, want true", realRoot)
	}
	if isKnownBrowserProfileRoot(t.TempDir()) {
		t.Fatal("a temp dir must not be flagged as a real browser profile root")
	}
	if err := EnsureSafeUserDataDir(realRoot, false); err == nil {
		t.Fatal("must refuse to launch against the real Chrome profile root without the override")
	}
}

func TestIsInsideRealBrowserProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	if err := os.MkdirAll(filepath.Join(root, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "decoy")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, dir := range []string{
		root,
		strings.ToUpper(root),
		filepath.Join(root, "Default"),
		filepath.Join(root, "Not Yet Created", "Deeper"),
		link,
		filepath.Join(link, "Default"),
		filepath.Join(link, "missing"),
		filepath.Join(home, ".config", "google-chrome", "Profile 1"),
	} {
		if !IsInsideRealBrowserProfile(dir) {
			t.Errorf("IsInsideRealBrowserProfile(%q) = false, want true", dir)
		}
	}
	for _, dir := range []string{
		"",
		filepath.Join(home, ".brw", "profiles", "bookkeeper"),
		filepath.Join(home, "Library", "Application Support", "Google", "Chrome-agent"),
		t.TempDir(),
	} {
		if IsInsideRealBrowserProfile(dir) {
			t.Errorf("IsInsideRealBrowserProfile(%q) = true, want false", dir)
		}
	}
}

func TestLaunchArgsHeadlessAndExtensions(t *testing.T) {
	base := LaunchConfig{UserDataDir: "/tmp/brw-agent", Port: 9333}
	tests := []struct {
		name    string
		mutate  func(*LaunchConfig)
		want    []string
		notWant []string
	}{
		{
			name:    "headed by default",
			mutate:  func(*LaunchConfig) {},
			notWant: []string{"--headless=new"},
		},
		{
			name:   "headless emits the new-headless form",
			mutate: func(c *LaunchConfig) { c.Headless = true },
			want:   []string{"--headless=new"},
		},
		{
			name: "headless still loads extensions",
			mutate: func(c *LaunchConfig) {
				c.Headless = true
				c.Extensions = []string{"/opt/brw/extension"}
			},
			want: []string{"--headless=new", "--load-extension=/opt/brw/extension"},
		},
		{
			name: "operator args come after ours so they win",
			mutate: func(c *LaunchConfig) {
				c.Headless = true
				c.Args = []string{"--window-size=1280,900"}
			},
			want: []string{"--window-size=1280,900"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			args := launchArgs(cfg, cfg.Port)
			joined := strings.Join(args, " ")
			for _, w := range tt.want {
				if !strings.Contains(joined, w) {
					t.Errorf("args missing %q\ngot: %s", w, joined)
				}
			}
			for _, n := range tt.notWant {
				if strings.Contains(joined, n) {
					t.Errorf("args unexpectedly contain %q\ngot: %s", n, joined)
				}
			}
			if got := args[len(args)-1]; got != "about:blank" {
				t.Errorf("last arg = %q, want about:blank", got)
			}
			if tt.name == "operator args come after ours so they win" {
				if strings.LastIndex(joined, "--window-size=") != strings.Index(joined, "--window-size=1280,900") {
					t.Errorf("the operator's --window-size must be the last one so it wins\ngot: %s", joined)
				}
			}
		})
	}
}
