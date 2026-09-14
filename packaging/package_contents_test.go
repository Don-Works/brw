package packaging

import (
	"os"
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
