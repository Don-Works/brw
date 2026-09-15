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

func browserManifest(t *testing.T, mint, teardown string) map[string]any {
	t.Helper()
	return map[string]any{
		"schema_version": ManifestSchemaVersion,
		"id":             "local.standin",
		"name":           "Local stand-in browser",
		"version":        "1.0.0",
		"description":    "Mints a CDP endpoint",
		"capabilities":   []string{CapabilityBrowserProvider},
		"browser": map[string]any{
			"kind":     BrowserKindExec,
			"command":  []string{mint},
			"teardown": []string{teardown, SessionToken},
		},
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
		"browser grant without the browser block": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["capabilities"] = []string{CapabilityBrowserProvider}
				delete(manifest, "credential")
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "requires a browser block",
		},
		"provider without the grant": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["capabilities"] = []string{CapabilityBrowserProvider}
				manifest["browser"] = map[string]any{
					"kind": BrowserKindExec, "command": []string{"/bin/echo"},
					"teardown": []string{"/bin/echo", SessionToken},
				}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "requires the credential.read capability",
		},
		"browser block without the browser grant": {
			setup: func(t *testing.T, root, credentials string) {
				manifest := fileManifest(credentials)
				manifest["browser"] = map[string]any{
					"kind": BrowserKindExec, "command": []string{"/bin/echo"},
					"teardown": []string{"/bin/echo", SessionToken},
				}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "requires the browser.provider capability",
		},
		"two browser holders": {
			setup: func(t *testing.T, root, credentials string) {
				first := browserManifest(t, "/bin/echo", "/bin/echo")
				writeManifest(t, root, "a.json", first)
				second := browserManifest(t, "/bin/echo", "/bin/echo")
				second["id"] = "other.browsers"
				writeManifest(t, root, "b.json", second)
			},
			wantErr: "only one plugin may",
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
		"exec program spelled by the reference": {
			setup: func(t *testing.T, root, _ string) {
				manifest := fileManifest("")
				manifest["credential"] = map[string]any{"kind": CredentialKindExec, "command": []string{"/usr/local/bin/" + ReferenceToken, "read"}}
				writeManifest(t, root, "a.json", manifest)
			},
			wantErr: "must not contain the " + ReferenceToken + " token",
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
		atLoad    bool
		wantErr   string
	}{
		// A program that is not there is a startup failure now that the loader
		// pins the binary: a daemon that boots with a provider it can never run
		// discovers that at the password field.
		"program missing": {
			program: filepath.Join(t.TempDir(), "no-such-vault-cli"), arguments: []string{ReferenceToken},
			atLoad: true, wantErr: "resolve credential command program",
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
			if test.atLoad {
				if err == nil {
					t.Fatalf("%s loaded", name)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s load error = %q, want one containing %q", name, err, test.wantErr)
				}
				return
			}
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

// The manifest an operator reviewed has to determine which binary runs. A bare
// name is resolved from the daemon's PATH at every call, and a group-writable
// program is chosen by whoever can write it, not by the manifest.
func TestLoadPinsTheBinaryAnExecProviderRuns(t *testing.T) {
	// Chmod after the write, not a mode argument: the process umask clears the
	// group and other bits this table is about.
	writeProgram := func(t *testing.T, dir, name string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makeDir := func(t *testing.T, path string, mode os.FileMode) {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, test := range map[string]struct {
		program func(t *testing.T) string
		wantErr string
	}{
		"bare name off the daemon PATH": {
			program: func(*testing.T) string { return "vaultcli" },
			wantErr: "must be an absolute path",
		},
		"absolute but unclean": {
			program: func(t *testing.T) string {
				dir := t.TempDir()
				writeProgram(t, dir, "vaultcli", 0o700)
				// Not filepath.Join, which would clean the path back out.
				return dir + "/sub/../vaultcli"
			},
			wantErr: "must already be a clean path",
		},
		"group-writable program": {
			program: func(t *testing.T) string { return writeProgram(t, t.TempDir(), "vaultcli", 0o720) },
			wantErr: "writable by group or other",
		},
		"program in a world-writable directory": {
			program: func(t *testing.T) string {
				parent := filepath.Join(t.TempDir(), "bin")
				makeDir(t, parent, 0o777)
				return writeProgram(t, parent, "vaultcli", 0o700)
			},
			wantErr: "is writable by group or other",
		},
		"program is a directory": {
			program: func(t *testing.T) string { return t.TempDir() },
			wantErr: "not a regular file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := fileManifest("")
			manifest["credential"] = map[string]any{
				"kind":    CredentialKindExec,
				"command": []string{test.program(t), ReferenceToken},
			}
			writeManifest(t, root, "exec.json", manifest)
			if _, err := Load(root); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load error = %v, want one containing %q", err, test.wantErr)
			}
		})
	}
}

// Every credential kind reaches credential.read through the same registry, so a
// kind classified by one switch over Kind and missed by the other — or accepted
// by both and trust-checked by neither — is how a gate stops covering what it
// gates. This enumerates the domain rather than naming the kinds that existed
// when it was written: a kind added to CredentialKinds with no row below fails
// here before it can ship.
func TestEveryCredentialKindIsClassifiedAndHeldToTheTrustBoundary(t *testing.T) {
	// One spec per kind, each naming a location another local user can write.
	// The enclosing directory is the daemon's own, so the only thing a row can
	// trip on is the location its spec points at.
	untrustedSpec := map[string]func(t *testing.T, shared string) CredentialProviderSpec{
		CredentialKindExec: func(t *testing.T, shared string) CredentialProviderSpec {
			t.Helper()
			program := filepath.Join(shared, "vaultcli")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			return CredentialProviderSpec{Kind: CredentialKindExec, Command: []string{program, ReferenceToken}}
		},
		CredentialKindFile: func(_ *testing.T, shared string) CredentialProviderSpec {
			return CredentialProviderSpec{Kind: CredentialKindFile, Directory: shared}
		},
	}
	for _, kind := range CredentialKinds {
		t.Run(kind, func(t *testing.T) {
			build, ok := untrustedSpec[kind]
			if !ok {
				t.Fatalf("credential kind %q has no row here; it reaches %s like every other kind, so say how it is held to the trust boundary", kind, CapabilityCredentialRead)
			}
			shared := filepath.Join(t.TempDir(), "shared")
			if err := os.Mkdir(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			// Chmod after Mkdir: the process umask clears the bits under test.
			if err := os.Chmod(shared, 0o777); err != nil {
				t.Fatal(err)
			}
			spec := build(t, shared)
			if err := validateCredentialSpec(spec); err != nil {
				t.Fatalf("validateCredentialSpec refused a well-formed %s spec: %v", kind, err)
			}
			provider, err := newCredentialProvider(spec)
			if err == nil {
				t.Fatalf("newCredentialProvider built a %s provider (%T) from a world-writable location", kind, provider)
			}
			if !strings.Contains(err.Error(), "writable by group or other") {
				t.Fatalf("%s provider from a world-writable location = %v, want the trust refusal", kind, err)
			}
		})
	}

	// A kind off the list reaches the default of both switches, and neither may
	// build anything from it.
	unknown := CredentialProviderSpec{Kind: "vault-agent", Directory: t.TempDir()}
	if err := validateCredentialSpec(unknown); err == nil || !strings.Contains(err.Error(), "credential kind must be one of") {
		t.Fatalf("validateCredentialSpec(%q) = %v, want a refusal naming the domain", unknown.Kind, err)
	}
	if _, err := newCredentialProvider(unknown); err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Fatalf("newCredentialProvider(%q) = %v, want a refusal", unknown.Kind, err)
	}
}

// docs/plugins.md states the trust boundary as the filesystem permission on the
// plugin directory. A 0700 directory inside a world-writable parent is not
// protected by its own mode: the parent's writers rename it away and put their
// own directory, with their own manifests, in its place.
func TestLoadRefusesAPluginDirectoryInsideAWritableParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "outer")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "plugins")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod after Mkdir: the process umask clears the bits under test.
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, "a.json", fileManifest(t.TempDir()))
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("Load error = %v, want a refusal naming the writable ancestor", err)
	}
	// The sticky bit is what makes a shared temporary directory safe as a
	// parent, and it is the reason the walk cannot simply refuse mode 0777.
	if err := os.Chmod(parent, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatalf("Load under a sticky world-writable parent = %v, want it accepted", err)
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
