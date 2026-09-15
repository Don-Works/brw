package chromeoptin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// The lane against a Chrome that has actually been opted in, rather than
// against a fixture shaped like one.
//
// This is what an earlier round deferred as untestable, on the grounds that no
// flag or preference available to a test produces the opt-in. Both halves of
// that were wrong: "user-enabled" under devtools.remote_debugging in Local
// State is Chrome's own preference — the one chrome://inspect/#remote-debugging
// writes — and a Chrome started with it writes DevToolsActivePort and serves no
// DevTools HTTP endpoint at all. Discovery asked /json/version, read the 404 as
// "the opt-in is off", and told the user to turn on something they had already
// turned on.
//
// Only a throwaway user data directory is ever touched. Nothing here goes near
// a real profile, and nothing turns the opt-in on anywhere a person would
// notice: the directory is deleted with the test.
func TestDiscoverFindsAChromeThatIsGenuinelyOptedIn(t *testing.T) {
	chromePath, err := cdplaunch.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dir := t.TempDir()
	localState := `{"devtools":{"remote_debugging":{"user-enabled":true}}}`
	if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte(localState), 0o600); err != nil {
		t.Fatalf("write Local State: %v", err)
	}

	// Deliberately no --remote-debugging-port: the preference is the only
	// reason this Chrome has an endpoint, which is what makes it this lane and
	// not a flavour of direct CDP.
	cmd := exec.CommandContext(ctx, chromePath,
		"--headless=new",
		"--disable-gpu",
		"--user-data-dir="+dir,
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start Chrome: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	activePort := filepath.Join(dir, activePortFile)
	recordedPort := 0
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(activePort)
		if readErr == nil {
			first, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
			if port, convErr := strconv.Atoi(strings.TrimSpace(first)); convErr == nil && port > 0 {
				recordedPort = port
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if recordedPort == 0 {
		// A Chrome older than the opt-in ignores the preference, and so does
		// one whose chosen port is already taken. Neither is a failure of the
		// code under test.
		t.Skipf("this Chrome recorded no debugging endpoint for the opt-in preference; it predates Chrome %d or could not bind its port", MinimumChromeMajor)
	}

	endpoint, err := Discover(ctx, Options{UserDataDir: dir})
	if err != nil {
		t.Fatalf("discovery refused a Chrome whose opt-in is on: %v", err)
	}
	if endpoint.Port != recordedPort {
		t.Fatalf("discovered port %d, but the browser recorded %d", endpoint.Port, recordedPort)
	}
	want := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/", recordedPort)
	if !strings.HasPrefix(endpoint.BrowserWSURL, want) {
		t.Fatalf("BrowserWSURL = %q, want one starting %q: the browser target is the whole of what this lane adds", endpoint.BrowserWSURL, want)
	}
	if endpoint.UserDataDir != dir {
		t.Fatalf("UserDataDir = %q, want %q", endpoint.UserDataDir, dir)
	}
}
