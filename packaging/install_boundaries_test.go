package packaging

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCopyFailurePreservesInstalledDirectories(t *testing.T) {
	for _, mode := range []string{"payload", "profile", "success"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			install := filepath.Join(root, "install")
			fake := filepath.Join(root, "fake")
			write := func(path, body string, perm os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), perm); err != nil {
					t.Fatal(err)
				}
			}
			var raw bytes.Buffer
			zipped := gzip.NewWriter(&raw)
			archive := tar.NewWriter(zipped)
			for name, body := range map[string]string{
				"bin/brw": "new brw", "bin/brwd": "new brwd", "bin/brwctl": "#!/bin/sh\necho brwctl\n", "bin/brwcheck": "new brwcheck", "bin/brw-devtools-mcp": "new devtools",
				"extension/manifest.json": "new manifest", "extension/background.js": "new extension", "extension/bridge-defaults.json": "packaged defaults",
			} {
				if err := archive.WriteHeader(&tar.Header{Name: "brw_1.2.3_linux_amd64/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}
				if _, err := archive.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err := zipped.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "archive.tar.gz"), raw.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(root, "archive.sha256"), fmt.Sprintf("%x  brw_1.2.3_linux_amd64.tar.gz\n", sha256.Sum256(raw.Bytes())), 0o600)
			write(filepath.Join(fake, "uname"), "#!/bin/sh\ncase $1 in -s) echo Linux;; -m) echo x86_64;; esac\n", 0o755)
			write(filepath.Join(fake, "curl"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in -o) shift; dest=$1;; *) url=$1;; esac
  shift
done
case "$url" in *.sha256) /bin/cp "$BRW_INSTALL_FIXTURE/archive.sha256" "$dest";; *) /bin/cp "$BRW_INSTALL_FIXTURE/archive.tar.gz" "$dest";; esac
`, 0o755)
			write(filepath.Join(fake, "cp"), `#!/bin/sh
for arg in "$@"; do
  if [ "$BRW_FAIL_COPY" = payload ]; then
    case "$arg" in */unpack/*/bin|*/unpack/*/bin/.) exit 7;; esac
  fi
done
if [ "$BRW_FAIL_COPY" = profile ] && [ "$2" = "$BRW_INSTALL_DIR/extension" ]; then exit 7; fi
exec /bin/cp "$@"
`, 0o755)
			write(filepath.Join(install, "bin/brwd"), "old daemon", 0o755)
			write(filepath.Join(install, "extension/bridge-defaults.json"), "global defaults", 0o600)
			write(filepath.Join(install, "extension-empty/manifest.json"), "old empty extension", 0o600)
			write(filepath.Join(install, "extension-profile/manifest.json"), "old profile extension", 0o600)
			write(filepath.Join(install, "extension-profile/bridge-defaults.json"), "profile defaults", 0o600)
			cmd := exec.Command("sh", "../scripts/install.sh")
			cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"), "BRW_VERSION=1.2.3", "BRW_BASE_URL=https://example.test/release", "BRW_INSTALL_DIR="+install, "BRW_INSTALL_FIXTURE="+root, "BRW_FAIL_COPY="+mode, "BRW_SKIP_ATTESTATION=1", "BRW_NO_SETUP=1", "BRW_NO_PATH=1")
			output, err := cmd.CombinedOutput()
			if mode != "success" && err == nil {
				t.Fatalf("failed copy accepted: %s", output)
			}
			if mode == "success" && err != nil {
				t.Fatalf("valid install failed: %v %s", err, output)
			}
			path, want := "bin/brwd", "old daemon"
			if mode == "success" {
				path, want = "extension-profile/manifest.json", "new manifest"
			}
			if mode == "profile" {
				path, want = "extension-profile/manifest.json", "old profile extension"
			}
			got, err := os.ReadFile(filepath.Join(install, path))
			if err != nil || string(got) != want {
				t.Fatalf("copy failure destroyed prior install: %q %v\n%s", got, err, output)
			}
			if mode == "profile" || mode == "success" {
				got, err := os.ReadFile(filepath.Join(install, "extension-profile/bridge-defaults.json"))
				if err != nil || string(got) != "profile defaults" {
					t.Fatalf("profile defaults lost: %q %v", got, err)
				}
			}
			if mode == "success" {
				got, err := os.ReadFile(filepath.Join(install, "extension/bridge-defaults.json"))
				if err != nil || string(got) != "global defaults" {
					t.Fatalf("global defaults lost: %q %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(install, "extension-empty/bridge-defaults.json")); !os.IsNotExist(err) {
					t.Fatalf("profile inherited another endpoint: %v", err)
				}
			}
			entries, err := os.ReadDir(install)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".brw-replace-") {
					t.Errorf("failed copy left staging directory %s", entry.Name())
				}
			}
		})
	}
}
