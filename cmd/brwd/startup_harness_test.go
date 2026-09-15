package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A guard that main() never calls decides nothing, and main() has no seam a
// unit test can reach: the decisions are taken inline between flag parsing and
// the browser launch. So the tests that prove the wiring run the real binary.
//
// This used to be a strings.Contains over main.go's own source, which passes on
// a call moved into a branch that never executes and on a call that survives
// only inside a comment. Running the binary is what tells those apart.

var (
	sharedBrwdBinary string
	sharedBrwdDir    string
	sharedBrwdErr    error
)

func TestMain(m *testing.M) {
	sharedBrwdDir, sharedBrwdErr = os.MkdirTemp("", "brwd-startup")
	if sharedBrwdErr == nil {
		sharedBrwdBinary = filepath.Join(sharedBrwdDir, "brwd")
		build := exec.Command("go", "build", "-o", sharedBrwdBinary, ".")
		if out, err := build.CombinedOutput(); err != nil {
			sharedBrwdErr = fmt.Errorf("build brwd: %v\n%s", err, out)
		}
	}
	code := m.Run()
	if sharedBrwdDir != "" {
		_ = os.RemoveAll(sharedBrwdDir)
	}
	os.Exit(code)
}

// brwdBinary is the daemon under test, built once for the whole package.
func brwdBinary(t *testing.T) string {
	t.Helper()
	if sharedBrwdErr != nil {
		t.Fatal(sharedBrwdErr)
	}
	return sharedBrwdBinary
}

// startupEnvironment is a clean environment for a daemon under test: this
// machine's own BRW_* settings are dropped, because a test whose result depends
// on the developer's exported BRW_IDLE_EXIT is testing the developer.
func startupEnvironment(home string, extra ...string) []string {
	out := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "BRW_") {
			continue
		}
		if home != "" && strings.HasPrefix(entry, "HOME=") {
			continue
		}
		out = append(out, entry)
	}
	if home != "" {
		out = append(out, "HOME="+home)
	}
	return append(out, extra...)
}

// runBrwdUntilItStops runs the daemon and returns its combined output and exit
// code. It is for invocations that refuse at startup: an invocation that got as
// far as serving would sit here until the deadline.
func runBrwdUntilItStops(t *testing.T, args, env []string, deadline time.Duration) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, brwdBinary(t), args...)
	cmd.Env = env
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run brwd %v: %v\n%s", args, err, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("brwd %v did not stop within %s; it was expected to refuse at startup\n%s", args, deadline, output.String())
	}
	return output.String(), code
}

// freePort returns a loopback port nothing is listening on. A daemon under test
// needs a concrete --http address, and hard-coding one makes two tests (or two
// agents on this machine) fight over it.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// fakeDevToolsEndpoint answers /json/version as a browser would, which is what
// --remote auto verifies before it attaches to anything. Only the discovery
// half is real: the WebSocket it names is not served, so a daemon that gets
// past the policy gate fails on the connection rather than driving anything.
func fakeDevToolsEndpoint(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/141.0.0.0","webSocketDebuggerUrl":"ws://%s/devtools/browser/fake"}`, r.Host)
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("the stub endpoint is not on a loopback port: %s", server.URL)
	}
	return server, port
}

// writeActivePort puts a browser's ephemeral debugging port where the browser
// itself would write it.
func writeActivePort(t *testing.T, dir string, port int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(fmt.Sprintf("%d\n/devtools/browser/fake\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeBridgeOnlyPolicy writes a profile policy barring direct CDP, which is
// the policy --remote auto has to honour.
func writeBridgeOnlyPolicy(t *testing.T, dir, userDataDir string) string {
	t.Helper()
	path := filepath.Join(dir, "browser-profiles.json")
	body := fmt.Sprintf(`{"profiles":[{"name":"chrome-work","user_data_dir":%q,"profile_directory":"Profile 1","direct_cdp_allowed":false,"extension_bridge_allowed":true}]}`, userDataDir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
