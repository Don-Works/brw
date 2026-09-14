//go:build !windows

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeShellProgram writes a script that prints one line, chmodded after the
// write because the process umask clears the bits these tests are about.
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

// A symlink is a second spelling of the program's location, and whoever can
// write the directory holding it chooses what it points at. Checking only the
// file the link resolves to therefore checks the wrong thing: a 0700 binary in
// a 0700 directory, named through a world-writable directory, is still a
// program another local user picks. Both ancestor chains have to be walked.
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

// The manifest an operator reviewed decides what runs, so what runs is the path
// the loader resolved and checked — not the declared name resolved a second
// time at exec, which is a second chance for the answer to change.
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

// brwd runs for weeks. A program that became group-writable at noon is chosen
// by whoever made it so, and a check that ran only at startup would still be
// reporting the answer it got at boot.
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
