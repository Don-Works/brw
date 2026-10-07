//go:build !windows

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeShellProgram(t *testing.T, path, output string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+output+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func execManifestRoot(t *testing.T, program string) string {
	t.Helper()
	root := t.TempDir()
	manifest := fileManifest("")
	manifest["credential"] = map[string]any{
		"kind":    CredentialKindExec,
		"command": []string{program, ReferenceToken},
	}
	writeManifest(t, root, "exec.json", manifest)
	return root
}

func TestLoadRefusesAnExecProgramNamedThroughAWritableDirectory(t *testing.T) {
	safe := t.TempDir()
	program := writeShellProgram(t, filepath.Join(safe, "vaultcli"), "honest-fixture-value", 0o700)

	shared := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shared, "op")
	symlinkOrSkip(t, program, link)

	_, err := Load(execManifestRoot(t, link))
	if err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("Load of a program named through a world-writable directory = %v, want a refusal naming the writable location", err)
	}
}

func TestExecProviderRunsTheProgramTheLoaderChecked(t *testing.T) {
	dir := t.TempDir()
	honest := writeShellProgram(t, filepath.Join(dir, "honest"), "honest-fixture-value", 0o700)
	other := writeShellProgram(t, filepath.Join(dir, "other"), "other-fixture-value", 0o700)
	link := filepath.Join(dir, "op")
	symlinkOrSkip(t, honest, link)

	registry, err := Load(execManifestRoot(t, link))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := registry.Resolve(context.Background(), "work/login")
	if err != nil {
		t.Fatal(err)
	}
	if secret.Reveal() != "honest-fixture-value" {
		t.Fatalf("exec provider returned %q before the link moved", secret.Reveal())
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, other, link)
	secret, err = registry.Resolve(context.Background(), "work/login")
	if err != nil {
		t.Fatalf("Resolve after the declared name moved = %v", err)
	}
	if secret.Reveal() != "honest-fixture-value" {
		t.Fatalf("exec provider returned %q; it re-resolved the declared name instead of running the program the loader checked", secret.Reveal())
	}
}

func TestExecProviderRefusesAProgramThatBecameWritableAfterLoad(t *testing.T) {
	dir := t.TempDir()
	program := writeShellProgram(t, filepath.Join(dir, "vaultcli"), "honest-fixture-value", 0o700)
	registry, err := Load(execManifestRoot(t, program))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(context.Background(), "work/login"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(program, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = registry.Resolve(context.Background(), "work/login")
	if err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("Resolve through a program that became world-writable = %v, want the same refusal Load would give", err)
	}
}
