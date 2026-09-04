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
