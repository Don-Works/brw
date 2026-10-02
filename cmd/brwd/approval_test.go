package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func approvalFixture(t *testing.T) approvalOptions {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "operator.token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 48)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return approvalOptions{
		enabled: true, siteConsent: true, mode: "risky", httpAddr: "127.0.0.1:17310",
		tokenFile: tokenPath, policyPath: filepath.Join(dir, "browser-profiles.json"), artifactDir: "off",
	}
}

func TestApprovalConfigurationRefusesIncompleteGates(t *testing.T) {
	cases := []struct {
		name string
		edit func(*approvalOptions)
	}{
		{"missing site consent", func(o *approvalOptions) { o.siteConsent = false }},
		{"interactive terminal", func(o *approvalOptions) { o.prompt = true }},
		{"upstream proxy", func(o *approvalOptions) { o.upstream = "http://127.0.0.1:17310" }},
		{"missing token", func(o *approvalOptions) { o.tokenFile = "" }},
		{"invalid mode", func(o *approvalOptions) { o.mode = "sometimes" }},
		{"http off", func(o *approvalOptions) { o.httpAddr = "off" }},
		{"public listener", func(o *approvalOptions) { o.httpAddr = "0.0.0.0:17310" }},
		{"remote listener", func(o *approvalOptions) { o.httpAddr = "192.0.2.1:17310" }},
		{"missing port", func(o *approvalOptions) { o.httpAddr = "127.0.0.1" }},
		{"dynamic port", func(o *approvalOptions) { o.httpAddr = "127.0.0.1:0" }},
		{"bad port", func(o *approvalOptions) { o.httpAddr = "127.0.0.1:65536" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			opts := approvalFixture(t)
			test.edit(&opts)
			if _, _, err := buildApprovalStore(opts); err == nil {
				t.Fatal("unsafe approval configuration accepted")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(opts.policyPath), "approvals")); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration created approval storage: %v", err)
			}
		})
	}
}

func TestApprovalOffDoesNotReadFilesOrCreateStorage(t *testing.T) {
	opts := approvalOptions{mode: "risky", policyPath: filepath.Join(t.TempDir(), "missing", "policy.json")}
	store, token, err := buildApprovalStore(opts)
	if err != nil || store != nil || token != "" {
		t.Fatalf("approval-off configuration: store=%v token=%q err=%v", store, token, err)
	}
	for _, edit := range []func(*approvalOptions){
		func(o *approvalOptions) { o.tokenFile = "/missing/token" },
		func(o *approvalOptions) { o.storePath = "/missing/store" },
		func(o *approvalOptions) { o.modeSet = true },
		func(o *approvalOptions) { o.mode = "all" },
	} {
		changed := opts
		edit(&changed)
		if err := validateApprovalOptions(changed); err == nil {
			t.Fatal("ignored approval option accepted without --approvals")
		}
	}
}

func TestApprovalStoreUsesPrivateDirectoryBesidePolicy(t *testing.T) {
	opts := approvalFixture(t)
	store, token, err := buildApprovalStore(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if token != strings.Repeat("a", 48) {
		t.Fatal("operator token was not loaded")
	}
	info, err := os.Lstat(filepath.Join(filepath.Dir(opts.policyPath), "approvals"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("approval directory is not private: info=%v err=%v", info, err)
	}
}

func TestApprovalTokenRefusesUnsafeFilesWithoutDisclosingContents(t *testing.T) {
	secret := strings.Repeat("DO-NOT-DISCLOSE", 4)
	cases := []struct {
		name string
		mode os.FileMode
		data string
		kind string
	}{
		{"public permissions", 0o644, secret, "file"},
		{"owner read only", 0o400, secret, "file"},
		{"short token", 0o600, "short", "file"},
		{"spaces in token", 0o600, secret + " token", "file"},
		{"unicode token", 0o600, secret + "é", "file"},
		{"oversized token", 0o600, strings.Repeat("a", 4097), "file"},
		{"symlink token", 0o600, secret, "symlink"},
		{"directory token", 0o700, secret, "directory"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			switch test.kind {
			case "file":
				if err := os.WriteFile(path, []byte(test.data), test.mode); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(dir, "real-token")
				if err := os.WriteFile(target, []byte(test.data), test.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, test.mode); err != nil {
					t.Fatal(err)
				}
			}
			_, err := readApprovalToken(path, "")
			if err == nil {
				t.Fatal("unsafe operator token accepted")
			}
			if strings.Contains(err.Error(), "DO-NOT-DISCLOSE") {
				t.Fatal("token contents disclosed in error")
			}
		})
	}
}

func TestApprovalStorageRefusesPublicAndSymlinkPaths(t *testing.T) {
	for _, kind := range []string{"public parent", "symlink parent", "public file", "symlink file"} {
		t.Run(kind, func(t *testing.T) {
			opts := approvalFixture(t)
			dir := filepath.Join(filepath.Dir(opts.policyPath), "approval-storage")
			opts.storePath = filepath.Join(dir, "requests.json")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public parent":
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink parent":
				alias := filepath.Join(filepath.Dir(dir), "approval-alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				opts.storePath = filepath.Join(alias, "requests.json")
			case "public file":
				if err := os.WriteFile(opts.storePath, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink file":
				if err := os.Symlink(opts.tokenFile, opts.storePath); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := buildApprovalStore(opts); err == nil {
				t.Fatal("unsafe approval storage accepted")
			}
		})
	}
}

func TestApprovalCredentialsAndStoreCannotLiveInArtifacts(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "artifact-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "token"), filepath.Join(alias, "new", "requests.json")} {
		if err := rejectApprovalArtifactPath(path, root); err == nil {
			t.Fatalf("artifact path %q was accepted", path)
		}
	}
	if err := rejectApprovalArtifactPath(root+"-private/token", root); err != nil {
		t.Fatalf("separate sibling directory rejected: %v", err)
	}
	for _, field := range []string{"token", "store"} {
		t.Run(field, func(t *testing.T) {
			opts := approvalFixture(t)
			opts.artifactDir = filepath.Dir(opts.tokenFile)
			if field == "store" {
				opts.artifactDir = root
				opts.storePath = filepath.Join(alias, "new", "requests.json")
			}
			if _, _, err := buildApprovalStore(opts); err == nil {
				t.Fatal("approval data inside artifacts accepted")
			}
		})
	}
}
