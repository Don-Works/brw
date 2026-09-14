//go:build !windows

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// resolveWithin fails the test rather than hanging when a provider ignores its
// deadline, so a regression reports in seconds instead of at the package
// timeout.
func resolveWithin(t *testing.T, registry *Registry, reference string, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := registry.Resolve(context.Background(), reference)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("resolve of %q returned a credential", reference)
		}
		return err
	case <-time.After(limit):
		t.Fatalf("resolve of %q did not return within %s; the provider deadline is not a deadline", reference, limit)
		return nil
	}
}

// docs/plugins.md promises every provider call a deadline. exec.CommandContext
// alone does not give one: it kills the child at the deadline but Wait still
// blocks until every inherited writer closes the stdout pipe, so a provider
// that backgrounds a grandchild and exits 0 holds the step open for as long as
// the grandchild lives — and then reports its empty output as the answer.
func TestExecProviderDeadlineSurvivesAGrandchildHoldingStdout(t *testing.T) {
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		t.Skipf("no %s on this machine: %v", shell, err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "forking-vault-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 25 &\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manifest := fileManifest("")
	manifest["credential"] = map[string]any{
		"kind":       CredentialKindExec,
		"command":    []string{script, ReferenceToken},
		"timeout_ms": 300,
	}
	writeManifest(t, root, "exec.json", manifest)
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	err = resolveWithin(t, registry, "work/login", 10*time.Second)
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("resolve error = %q, want the named timeout; a provider that outran its deadline must not report anything else", err)
	}
}

// The file kind has a deadline too, which is the only thing that makes the
// doc's "every provider call" true. os.Stat and os.ReadFile take no context, so
// a named pipe with no writer — or a wedged network mount — would otherwise
// hold the recipe step open forever. This is also where timeout_ms on a file
// provider stops being a setting that validates and does nothing.
func TestFileProviderHonoursItsDeadlineOnAnUnreadableFile(t *testing.T) {
	credentials := t.TempDir()
	fifo := filepath.Join(credentials, "blocking")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no named pipes on this machine: %v", err)
	}
	root := t.TempDir()
	manifest := fileManifest(credentials)
	manifest["credential"] = map[string]any{
		"kind":       CredentialKindFile,
		"directory":  credentials,
		"timeout_ms": 200,
	}
	writeManifest(t, root, "local.json", manifest)
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	err = resolveWithin(t, registry, "blocking", 10*time.Second)
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("resolve error = %q, want the named timeout", err)
	}
}
