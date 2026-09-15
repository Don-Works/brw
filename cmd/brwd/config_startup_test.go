package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runBrwd builds and runs the real daemon with --print-system-prompt, which is
// the one invocation that reaches the startup path and then exits. Everything
// before that point — flag parsing, the config file, the precedence rule — is
// the same code every real launch runs.
func runBrwd(t *testing.T, args []string, env []string) (string, string, int) {
	t.Helper()
	binary := brwdBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append(args, "--print-system-prompt")...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run brwd: %v\n%s", err, stderr.String())
	}
	return stdout.String(), stderr.String(), code
}

// TestBrwdReadsBrwJSONAtStartup proves the wiring, not just the package: a
// config file that nothing in main() ever loaded would pass every test in
// internal/brwconfig and change nothing about a running daemon.
func TestBrwdReadsBrwJSONAtStartup(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "brw.json")
	if err := os.WriteFile(good, []byte(`{"defaults":{"mcp-tools":"core","idle-exit":"20m"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBrwd(t, []string{"--config", good}, nil)
	if code != 0 {
		t.Fatalf("brwd --config <good> exited %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "mcp-tools=core") || !strings.Contains(stderr, "idle-exit=20m") {
		t.Fatalf("brwd did not report applying the config file:\n%s", stderr)
	}
	if !strings.Contains(stdout, "brw tools") {
		t.Fatalf("brwd did not reach its normal startup path: %.200s", stdout)
	}

	// The environment still wins, which is the precedence rule where it
	// actually runs rather than in a unit test's flag set.
	_, stderr, code = runBrwd(t, []string{"--config", good}, []string{"BRW_MCP_TOOLS=minimal"})
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "mcp-tools=core") {
		t.Fatalf("the config file overrode BRW_MCP_TOOLS:\n%s", stderr)
	}
	if !strings.Contains(stderr, "idle-exit=20m") {
		t.Fatalf("the rest of the file stopped applying:\n%s", stderr)
	}

	// And the command line wins over both.
	_, stderr, code = runBrwd(t, []string{"--config", good, "--mcp-tools", "all"}, []string{"BRW_MCP_TOOLS=minimal"})
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "mcp-tools=") {
		t.Fatalf("the config file reported supplying a flag the command line set:\n%s", stderr)
	}

	// A misspelled key is a startup failure, not a setting that does nothing.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"defaults":{"headles":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runBrwd(t, []string{"--config", bad}, nil)
	if code == 0 {
		t.Fatal("brwd started with a config file naming a flag it does not have")
	}
	if !strings.Contains(stderr, "headles") {
		t.Fatalf("the failure does not name the key: %s", stderr)
	}

	// A file that would flip what this invocation IS is refused outright.
	dangerous := filepath.Join(dir, "dangerous.json")
	if err := os.WriteFile(dangerous, []byte(`{"defaults":{"unsafe-real-profile":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runBrwd(t, []string{"--config", dangerous}, nil)
	if code == 0 {
		t.Fatal("a config file switched on --unsafe-real-profile")
	}
	if !strings.Contains(stderr, "unsafe-real-profile") {
		t.Fatalf("the refusal does not name the flag: %s", stderr)
	}

	// BRW_CONFIG finds it too, so a service unit can point at one without
	// changing its arguments.
	_, stderr, code = runBrwd(t, nil, []string{"BRW_CONFIG=" + good})
	if code != 0 {
		t.Fatalf("BRW_CONFIG exited %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "mcp-tools=core") {
		t.Fatalf("BRW_CONFIG was not read:\n%s", stderr)
	}
}
