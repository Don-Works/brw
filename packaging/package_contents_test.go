package packaging

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallersBundlePublicAgentSkill(t *testing.T) {
	t.Parallel()

	requireFileContains(t, "linux/nfpm.yaml",
		`src: "@BRW_PACKAGE_ROOT@/usr/share/brw/skills"`,
		"dst: /usr/share/brw/skills",
	)
	requireFileContains(t, "../scripts/package-linux.sh",
		`cp -R "$repo_root/skills" "$root_dir/usr/share/brw/skills"`,
	)
	requireFileContains(t, "../scripts/package-macos.sh",
		`cp -R "$repo_root/skills" "$root_dir/usr/local/share/brw/skills"`,
	)
	requireFileContains(t, "../scripts/package-windows.ps1",
		`Copy-Item -Recurse -Force (Join-Path $RepoRoot "skills") (Join-Path $StageDir "share/skills")`,
	)

	requireFileContains(t, "../scripts/package-tarball.sh",
		`cp -R "$repo_root/skills" "$stage_dir/skills"`,
		`tar -C "$work_dir" -cf - "$name"`,
	)
	requireFileContains(t, "windows/brw.wxs",
		`<Files Directory="INSTALLFOLDER" Include="$(var.SourceDir)\**" />`,
	)

	for _, path := range []string{"../skills/brw/SKILL.md", "../skills/brw/references/recipes.md"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("public agent skill payload %s: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("public agent skill payload %s is not a regular file", path)
		}
	}
}

func TestInstallersShipTheBrwCLI(t *testing.T) {
	t.Parallel()

	requireFileContains(t, "linux/nfpm.yaml",
		`src: "@BRW_PACKAGE_ROOT@/usr/bin/brw"`,
		"dst: /usr/bin/brw",
		`src: "@BRW_PACKAGE_ROOT@/usr/bin/brw-testbed"`,
		"dst: /usr/bin/brw-testbed",
	)
	requireFileContains(t, "../scripts/package-linux.sh",
		"for cmd in brw brwd brwctl brwcheck brw-devtools-mcp brw-testbed; do",
	)
	requireFileContains(t, "../scripts/package-macos.sh",
		"binaries=(brw brwd brwctl brwcheck brw-devtools-mcp brw-testbed)",
	)
	requireFileContains(t, "../scripts/package-windows.ps1",
		`foreach ($CommandName in @("brw", "brwd", "brwctl", "brwcheck", "brw-devtools-mcp", "brw-testbed")) {`,
	)
	requireFileContains(t, "../scripts/package-tarball.sh",
		"for cmd in brw brwd brwctl brwcheck brw-devtools-mcp brw-testbed; do",
	)

	requireFileContains(t, "../scripts/install.sh",
		`COMMANDS="brw brwd brwctl brwcheck brw-devtools-mcp brw-testbed"`,
	)
	requireFileContains(t, "../Taskfile.yml",
		`- go build -ldflags "{{.GO_LDFLAGS}}" -o bin/brw ./cmd/brw`,
		`- cp bin/brw "{{.DATADIR}}/bin/brw"`,
		`- cp bin/brw "{{.MAC_APPDIR}}/bin/brw"`,
		`- go build -ldflags "{{.GO_LDFLAGS}}" -o bin/brw-testbed ./cmd/brw-testbed`,
		`- cp bin/brw-testbed "{{.DATADIR}}/bin/brw-testbed"`,
		`- cp bin/brw-testbed "{{.MAC_APPDIR}}/bin/brw-testbed"`,
	)
}

func TestInstallersBundleOptionalReader(t *testing.T) {
	t.Parallel()
	for _, target := range []struct{ name, dir string }{
		{"tarball", "$stage_dir/reader"},
		{"linux", "$root_dir/usr/share/brw/reader"},
		{"macos", "$root_dir/usr/local/share/brw/reader"},
	} {
		requireFileContains(t, "../scripts/package-"+target.name+".sh",
			`cp "$repo_root/scripts/browser-answer-worker.py" "$repo_root/scripts/browser-reader-mcp.py" "$repo_root/scripts/browser-reader-usage.py" "`+target.dir+`/"`)
	}
	requireFileContains(t, "linux/nfpm.yaml", `src: "@BRW_PACKAGE_ROOT@/usr/share/brw/reader"`, "dst: /usr/share/brw/reader")
	requireFileContains(t, "../scripts/package-windows.ps1",
		`Copy-Item -Force (Join-Path $RepoRoot "scripts/browser-answer-worker.py"), (Join-Path $RepoRoot "scripts/browser-reader-mcp.py"), (Join-Path $RepoRoot "scripts/browser-reader-usage.py") (Join-Path $StageDir "share/reader")`)
	requireFileContains(t, "../scripts/install.sh", `PAYLOAD="bin extension tests skills reader doc"`)
	requireFileContains(t, "../Taskfile.yml",
		`cp scripts/browser-answer-worker.py scripts/browser-reader-mcp.py scripts/browser-reader-usage.py "{{.DATADIR}}/reader/"`,
		`cp scripts/browser-answer-worker.py scripts/browser-reader-mcp.py scripts/browser-reader-usage.py "{{.MAC_APPDIR}}/reader/"`)
}

func TestTarballContainsRelocatableReader(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	root := t.TempDir()
	for _, dir := range []string{"scripts", "extension", "tests", "skills", "fake-bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scripts/package-tarball.sh", "scripts/browser-answer-worker.py", "scripts/browser-reader-mcp.py", "scripts/browser-reader-usage.py", "LICENSE", "README.md"} {
		data, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\nif [ \"$1\" = -o ]; then shift; printf binary > \"$1\"; exit; fi\nshift\ndone\nexit 1\n"
	if err := os.WriteFile(filepath.Join(root, "fake-bin", "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "package-tarball.sh"), "1.2.3", "linux", "amd64")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "fake-bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v: %s", err, out)
	}
	file, err := os.Open(filepath.Join(root, "dist", "release", "brw_1.2.3_linux_amd64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	found := map[string]bool{}
	testbedPayload := false
	readerDir := filepath.Join(root, "reader-smoke")
	if err := os.MkdirAll(readerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "brw_1.2.3_linux_amd64/bin/brw-testbed" && header.Typeflag == tar.TypeReg {
			testbedPayload = true
		}
		name := strings.TrimPrefix(header.Name, "brw_1.2.3_linux_amd64/reader/")
		if name == header.Name || header.Typeflag != tar.TypeReg {
			continue
		}
		if name != "browser-answer-worker.py" && name != "browser-reader-mcp.py" && name != "browser-reader-usage.py" {
			t.Fatalf("unexpected reader payload %q", name)
		}
		got, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("..", "scripts", name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("reader payload %s differs from source: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(readerDir, name), got, 0o644); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	if len(found) != 3 {
		t.Fatalf("archive reader files = %v, want worker, adapter and usage helper", found)
	}
	if !testbedPayload {
		t.Fatal("release archive omits brw-testbed")
	}
	t.Run("pythonEntrypoints", func(t *testing.T) {
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 unavailable")
		}
		for _, name := range []string{"browser-answer-worker.py", "browser-reader-mcp.py"} {
			cmd := exec.Command(python, filepath.Join(readerDir, name), "--help")
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("packaged reader %s: %v: %s", name, err, output)
			}
		}
	})
}

func requireFileContains(t *testing.T, path string, fragments ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(string(data), fragment) {
			t.Errorf("%s does not include required package rule %q", path, fragment)
		}
	}
}

func TestTarballSignsAfterCleanupAndPreservesKeychainArgument(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	if runtime.GOOS == "darwin" {
		bash = "/bin/bash"
	}
	for name, identity := range map[string]string{"ad_hoc": "-", "developer_id": "fixture identity"} {
		t.Run(name, func(t *testing.T) { testTarballSigner(t, bash, identity) })
	}
}

func testTarballSigner(t *testing.T, bash, identity string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"scripts", "extension", "tests", "skills", "fake-bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scripts/package-tarball.sh", "scripts/browser-answer-worker.py", "scripts/browser-reader-mcp.py", "scripts/browser-reader-usage.py", "LICENSE", "README.md"} {
		data, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"go": `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then shift; printf binary > "$1"; exit; fi
  shift
done
exit 1
`,
		"xattr": "#!/bin/sh\nrm -f \"$2\"/bin/*.signed\n",
		"codesign": `#!/bin/sh
arguments=$(printf '%s\n' "$@")
while [ "$#" -gt 0 ]; do
  last=$1
  shift
done
printf signed > "$last.signed"
printf '%s\n' "$arguments" > "$last.flags"
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(root, "fake-bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keychain := filepath.Join(root, "keychain with spaces")
	cmd := exec.Command(bash, filepath.Join(root, "scripts/package-tarball.sh"), "1.2.3", "darwin", "arm64")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "fake-bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "MACOS_SIGN_IDENTITY="+identity,
		"MACOS_KEYCHAIN="+keychain)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("signing package: %v: %s", err, output)
	}
	file, err := os.Open(filepath.Join(root, "dist/release/brw_1.2.3_darwin_arm64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	archive := tar.NewReader(zipped)
	signed := map[string]bool{}
	flags := map[string]string{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, ".signed") {
			signed[strings.TrimSuffix(filepath.Base(header.Name), ".signed")] = true
		}
		if strings.HasSuffix(header.Name, ".flags") {
			data, err := io.ReadAll(archive)
			if err != nil {
				t.Fatal(err)
			}
			flags[strings.TrimSuffix(filepath.Base(header.Name), ".flags")] = string(data)
		}
	}
	for _, command := range []string{"brw", "brwd", "brwctl", "brwcheck", "brw-devtools-mcp", "brw-testbed"} {
		if !signed[command] {
			t.Errorf("cleanup removed completed signature for %s", command)
		}
		want := []string{"--force", "--sign", identity}
		if identity != "-" {
			want = append(want, "--timestamp", "--options", "runtime", "--keychain", keychain)
		}
		want = append(want, filepath.Join(root, "dist/package/tarball-darwin-arm64/brw_1.2.3_darwin_arm64/bin", command))
		if flags[command] != strings.Join(want, "\n")+"\n" {
			t.Errorf("%s signing arguments=%q want=%q", command, flags[command], want)
		}
	}
}
