package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/credential"
)

// Low-entropy and obviously fabricated. Every non-leakage assertion in this
// repository searches for a literal like this one, so it must never look like
// a real secret.
const fixtureCredentialValue = "fixture-login-value-one"

func writeManifest(t *testing.T, dir, name string, manifest map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileManifest(credentialDir string) map[string]any {
	return map[string]any{
		"schema_version": ManifestSchemaVersion,
		"id":             "local.files",
		"name":           "Local credential files",
		"version":        "1.0.0",
		"description":    "One file per credential",
		"capabilities":   []string{CapabilityCredentialRead},
		"credential":     map[string]any{"kind": CredentialKindFile, "directory": credentialDir},
	}
}

// fileProviderRegistry builds the reference file provider with one credential
// in it, which is what lets the credential path run on a machine with no vault
// CLI installed at all.
func fileProviderRegistry(t *testing.T, reference, value string) *Registry {
	t.Helper()
	root := t.TempDir()
	credentials := t.TempDir()
	path := filepath.Join(credentials, filepath.FromSlash(reference))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, "local.json", fileManifest(credentials))
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestLoadRefusesManifestsThatWouldMoveTheTrustBoundary(t *testing.T) {
	for name, test := range map[string]struct {
		setup   func(t *testing.T, root, credentials string)
		wantErr string
	}{
		"group writable manifest": {
			setup: func(t *testing.T, root, credentials string) {
				path := writeManifest(t, root, "a.json", fileManifest(credentials))
				if err := os.Chmod(path, 0o620); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "writable by group or other",
		},
		"duplicate plugin id": {
			setup: func(t *testing.T, root, credentials string) {
				writeManifest(t, root, "a.json", fileManifest(credentials))
				writeManifest(t, root, "b.json", fileManifest(credentials))
			},
			wantErr: "already loaded",
		},
		"two credential holders": {
			setup: func(t *testing.T, root, credentials string) {
				writeManifest(t, root, "a.json", fileManifest(credentials))
				second := fileManifest(credentials)
				second["id"] = "other.files"
				writeManifest(t, root, "b.json", second)
			},
			wantErr: "only one plugin may",
		},
		"refused capability": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["capabilities"] = []string{CapabilityCredentialRead, "cookies.export"}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "widen a brw security default",
		},
		"reserved capability": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["capabilities"] = []string{CapabilityBrowserProvider}
				delete(manifest, "credential")
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "reserved",
		},
		"provider without the grant": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["capabilities"] = []string{CapabilityBrowserProvider}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "requires the credential.read capability",
		},
		"grant without the provider": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				delete(manifest, "credential")
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "requires a credential block",
		},
		"unknown manifest field": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["sandbox"] = true
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "not valid JSON",
		},
		"exec command without the reference token": {
			setup: func(t *testing.T, root, _ string) {
				manifest := fileManifest("")
				manifest["credential"] = map[string]any{"kind": CredentialKindExec, "command": []string{"vaultcli", "read"}}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "exactly once, found 0",
		},
		"exec command with two reference tokens": {
			setup: func(t *testing.T, root, _ string) {
				manifest := fileManifest("")
				manifest["credential"] = map[string]any{"kind": CredentialKindExec, "command": []string{"vaultcli", ReferenceToken, ReferenceToken}}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "exactly once, found 2",
		},
		"timeout past the ceiling": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["credential"] = map[string]any{"kind": CredentialKindFile, "directory": credentials, "timeout_ms": 600_000}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "timeout_ms must be between",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, credentials := t.TempDir(), t.TempDir()
			test.setup(t, root, credentials)
			_, err := Load(root)
			if err == nil {
				t.Fatalf("Load accepted a manifest it must refuse (%s)", name)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load error = %q, want one containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadRefusesAGroupWritableDirectory(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "a.json", fileManifest(t.TempDir()))
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("Load error = %v, want a refusal of the writable directory", err)
	}
}

func TestLoadWithNoDirectoryConfiguredFailsClosedOnResolve(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Plugins(); len(got) != 0 {
		t.Fatalf("empty registry reported %v", got)
	}
	if _, err := registry.Resolve(context.Background(), "login"); !errors.Is(err, credential.ErrNoProvider) {
		t.Fatalf("Resolve without a provider = %v, want ErrNoProvider", err)
	}
	if err := registry.ProbeProvider(); !errors.Is(err, credential.ErrNoProvider) {
		t.Fatalf("ProbeProvider without a provider = %v, want ErrNoProvider", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("Load accepted a plugin directory that does not exist")
	}
}

func TestFileProviderResolvesAndRefusesEveryWayOut(t *testing.T) {
	registry := fileProviderRegistry(t, "work/login", fixtureCredentialValue)
	secret, err := registry.Resolve(context.Background(), "work/login")
	if err != nil {
		t.Fatal(err)
	}
	if secret.Reveal() != fixtureCredentialValue {
		t.Fatalf("resolved %q, want the fixture value", secret.Reveal())
	}

	for name, reference := range map[string]string{
		"missing":       "work/absent",
		"directory":     "work",
		"parent escape": "../outside",
		"absolute":      "/etc/hosts",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := registry.Resolve(context.Background(), reference); err == nil {
				t.Fatalf("Resolve(%q) succeeded", reference)
			}
		})
	}
}

func TestFileProviderRefusesASharedReadableCredentialAndASymlinkOut(t *testing.T) {
	root, credentials, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeManifest(t, root, "local.json", fileManifest(credentials))
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}

	shared := filepath.Join(credentials, "shared")
	if err := os.WriteFile(shared, []byte(fixtureCredentialValue), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(context.Background(), "shared"); err == nil || !strings.Contains(err.Error(), "readable by group or other") {
		t.Fatalf("Resolve of a world-readable credential = %v", err)
	}

	target := filepath.Join(outside, "elsewhere")
	if err := os.WriteFile(target, []byte(fixtureCredentialValue), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(credentials, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := registry.Resolve(context.Background(), "link"); err == nil || !strings.Contains(err.Error(), "escapes the configured credential directory") {
		t.Fatalf("Resolve through a symlink out of the directory = %v", err)
	}
}

// The exec provider is what the 1Password manifest uses. `echo` stands in for
// `op read` so the argv substitution and the stdout contract are exercised
// without the test suite depending on a vault CLI being installed.
func TestExecProviderSubstitutesTheReferenceIntoOneArgument(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skipf("no echo on this machine: %v", err)
	}
	root := t.TempDir()
	manifest := fileManifest("")
	manifest["credential"] = map[string]any{
		"kind":    CredentialKindExec,
		"command": []string{echo, "prefix-" + ReferenceToken},
	}
	writeManifest(t, root, "exec.json", manifest)
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := registry.Resolve(context.Background(), "work/login")
	if err != nil {
		t.Fatal(err)
	}
	// echo appends a newline; the shared validation strips exactly one.
	if secret.Reveal() != "prefix-work/login" {
		t.Fatalf("exec provider returned %q, want the substituted argument", secret.Reveal())
	}
}

func TestExecProviderFailsClosedOnEveryProviderFailure(t *testing.T) {
	for name, test := range map[string]struct {
		program   string
		arguments []string
		timeoutMS int
		wantErr   string
	}{
		"program missing": {
			program: filepath.Join(t.TempDir(), "no-such-vault-cli"), arguments: []string{ReferenceToken},
			wantErr: "could not be run",
		},
		"non-zero exit":  {program: "false", arguments: []string{ReferenceToken}, wantErr: "exited with status"},
		"prints nothing": {program: "true", arguments: []string{ReferenceToken}, wantErr: "empty value"},
		"runs past its deadline": {
			program: "sleep", arguments: []string{ReferenceToken}, timeoutMS: 50, wantErr: "timed out",
		},
	} {
		t.Run(name, func(t *testing.T) {
			program := test.program
			if !filepath.IsAbs(program) {
				resolved, err := exec.LookPath(program)
				if err != nil {
					t.Skipf("no %s on this machine: %v", program, err)
				}
				program = resolved
			}
			root := t.TempDir()
			manifest := fileManifest("")
			spec := map[string]any{"kind": CredentialKindExec, "command": append([]string{program}, test.arguments...)}
			if test.timeoutMS > 0 {
				spec["timeout_ms"] = test.timeoutMS
			}
			manifest["credential"] = spec
			writeManifest(t, root, "exec.json", manifest)
			registry, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			// "30" is a legal reference name and, for sleep, thirty seconds.
			_, err = registry.Resolve(context.Background(), "30")
			if err == nil {
				t.Fatalf("%s resolved a credential", name)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("%s error = %q, want one containing %q", name, err, test.wantErr)
			}
		})
	}
}

func TestLoadRefusesAnOversizedManifest(t *testing.T) {
	root := t.TempDir()
	manifest := fileManifest(t.TempDir())
	manifest["description"] = strings.Repeat("x", MaxManifestBytes)
	writeManifest(t, root, "big.json", manifest)
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load error = %v, want a size refusal", err)
	}
}

func TestRevokeFailsClosedAndNamesThePlugin(t *testing.T) {
	registry := fileProviderRegistry(t, "work/login", fixtureCredentialValue)
	if _, err := registry.Resolve(context.Background(), "work/login"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Revoke("absent.plugin"); !errors.Is(err, ErrPluginNotLoaded) {
		t.Fatalf("Revoke of an unknown id = %v, want ErrPluginNotLoaded", err)
	}
	if err := registry.Revoke("local.files"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"resolve": func() error { _, err := registry.Resolve(context.Background(), "work/login"); return err }(),
		"probe":   registry.ProbeProvider(),
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(err, credential.ErrProviderRevoked) {
				t.Fatalf("after revoke, %s = %v, want ErrProviderRevoked", name, err)
			}
			if !strings.Contains(err.Error(), "local.files") {
				t.Fatalf("revoked error %q does not name the plugin", err)
			}
		})
	}
	status := registry.Plugins()
	if len(status) != 1 || !status[0].Revoked {
		t.Fatalf("plugin listing after revoke = %+v", status)
	}
}

// The listing is a control-plane answer. It says which backend kind answers so
// an operator can tell a vault CLI from a directory of files, and stops there:
// the daemon's own filesystem layout is not part of it.
func TestPluginListingCarriesNoFilesystemLayout(t *testing.T) {
	root, credentials := t.TempDir(), t.TempDir()
	writeManifest(t, root, "local.json", fileManifest(credentials))
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(registry.Plugins())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), credentials) || strings.Contains(string(encoded), root) {
		t.Fatalf("plugin listing %s carries a daemon filesystem path", encoded)
	}
	if !strings.Contains(string(encoded), CredentialKindFile) {
		t.Fatalf("plugin listing %s does not say which backend answers", encoded)
	}
}

func TestResolveRefusesAReferenceTheSchemaWouldNotProduce(t *testing.T) {
	registry := fileProviderRegistry(t, "work/login", fixtureCredentialValue)
	for _, reference := range []string{"", "work login", "work/../login", "work/./login", "work//login", "/work/login", strings.Repeat("a", 200)} {
		if _, err := registry.Resolve(context.Background(), reference); err == nil {
			t.Fatalf("Resolve(%q) succeeded", reference)
		}
	}
}
