package packaging

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// The second fragment is what makes the first load-bearing: staging the
	// skill only ships it because $stage_dir is the directory tar archives.
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

// A binary that ships from one installer and not the others is the failure
// this guards: brw is a separate command from brwd/brwctl, so every packaging
// target has to name it explicitly.
func TestInstallersShipTheBrwCLI(t *testing.T) {
	t.Parallel()

	requireFileContains(t, "linux/nfpm.yaml",
		`src: "@BRW_PACKAGE_ROOT@/usr/bin/brw"`,
		"dst: /usr/bin/brw",
	)
	requireFileContains(t, "../scripts/package-linux.sh",
		"for cmd in brw brwd brwctl brwcheck brw-devtools-mcp; do",
	)
	requireFileContains(t, "../scripts/package-macos.sh",
		"binaries=(brw brwd brwctl brwcheck brw-devtools-mcp)",
	)
	requireFileContains(t, "../scripts/package-windows.ps1",
		`foreach ($CommandName in @("brw", "brwd", "brwctl", "brwcheck", "brw-devtools-mcp")) {`,
	)
	requireFileContains(t, "../scripts/package-tarball.sh",
		"for cmd in brw brwd brwctl brwcheck brw-devtools-mcp; do",
	)
	// The one-line installer links what it lists; a binary missing from
	// COMMANDS is unpacked but never reaches PATH.
	requireFileContains(t, "../scripts/install.sh",
		`COMMANDS="brw brwd brwctl brwcheck brw-devtools-mcp"`,
	)
	requireFileContains(t, "../Taskfile.yml",
		`- go build -ldflags "{{.GO_LDFLAGS}}" -o bin/brw ./cmd/brw`,
		`- cp bin/brw "{{.DATADIR}}/bin/brw"`,
		`- cp bin/brw "{{.MAC_APPDIR}}/bin/brw"`,
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
			`cp "$repo_root/scripts/browser-answer-worker.py" "$repo_root/scripts/browser-reader-mcp.py" "`+target.dir+`/"`)
	}
	requireFileContains(t, "linux/nfpm.yaml", `src: "@BRW_PACKAGE_ROOT@/usr/share/brw/reader"`, "dst: /usr/share/brw/reader")
	requireFileContains(t, "../scripts/package-windows.ps1",
		`Copy-Item -Force (Join-Path $RepoRoot "scripts/browser-answer-worker.py"), (Join-Path $RepoRoot "scripts/browser-reader-mcp.py") (Join-Path $StageDir "share/reader")`)
	requireFileContains(t, "../scripts/install.sh", `PAYLOAD="bin extension tests skills reader doc"`)
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
	for _, name := range []string{"scripts/package-tarball.sh", "scripts/browser-answer-worker.py", "scripts/browser-reader-mcp.py", "LICENSE", "README.md"} {
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
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(header.Name, "brw_1.2.3_linux_amd64/reader/")
		if name == header.Name || header.Typeflag != tar.TypeReg {
			continue
		}
		if name != "browser-answer-worker.py" && name != "browser-reader-mcp.py" {
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
		found[name] = true
	}
	if len(found) != 2 {
		t.Fatalf("archive reader files = %v, want worker and adapter", found)
	}
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
