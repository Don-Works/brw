package cdp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLauncherCloseQuitsGracefullyBeforeKill verifies Close lets a
// SIGTERM-responsive process exit on its own (flushing its profile) well within
// the grace window, rather than hard-killing it — the corruption-avoiding path.
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

// TestLauncherCloseKillsAfterGrace verifies Close escalates to SIGKILL when the
// process ignores SIGTERM past the grace window (so shutdown can never hang),
// but only AFTER giving it the grace period.
func TestLauncherCloseKillsAfterGrace(t *testing.T) {
	// A process that hard-ignores SIGTERM, so only SIGKILL can end it. perl's
	// $SIG{TERM}="IGNORE" is reliable across macOS/Linux (shell `trap` is not).
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skipf("perl not available to model a SIGTERM-ignoring process: %v", err)
	}
	cmd := exec.Command(perl, "-e", `$SIG{TERM}="IGNORE"; sleep 30;`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Let perl install the SIG_IGN handler before we signal it; otherwise a TERM
	// landing during interpreter startup hits the default (terminate) disposition.
	// Chrome is long-running, so this race never exists in practice.
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

// TestEffectiveUserDataDirHonorsArgsOverride proves the profile-corruption guard
// validates the dir Chrome will ACTUALLY use, not the pre-args one: a
// --user-data-dir smuggled through passthrough args (which Chrome, keeping the
// last occurrence, would honor) must be the value checked.
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

// TestKnownBrowserProfileRootIsCaseInsensitive proves a case-variant of a real
// profile root is still recognised — macOS (APFS) and Windows are
// case-insensitive, so an exact-string compare would let a case-variant dodge
// the guard while opening the same profile.
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

// TestPathIdentitiesResolvesSymlinks proves the symlink-resolution that stops a
// symlink-to-the-real-profile from slipping past the exact-string comparison.
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
	// A SingletonLock symlinking to OUR live pid models a running Chrome holding
	// the profile.
	target := fmt.Sprintf("somehost-%d", os.Getpid())
	if err := os.Symlink(target, filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := EnsureSafeUserDataDir(dir, false)
	if err == nil {
		t.Fatal("expected refusal when a live Chrome owns the profile (SingletonLock -> live pid)")
	}
	// The live-lock guard must hold even with the real-profile override, since it
	// is about a *running* process, not which profile it is.
	if err := EnsureSafeUserDataDir(dir, true); err == nil {
		t.Fatal("live SingletonLock must be refused even with allowRealProfile=true")
	}
}

func TestEnsureSafeUserDataDirAllowsStaleSingletonLock(t *testing.T) {
	dir := t.TempDir()
	// Spawn and reap a process so its pid is now dead; a SingletonLock pointing at
	// it is stale and must NOT block launch (Chrome clears stale locks itself).
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
