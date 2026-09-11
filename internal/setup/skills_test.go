package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeInstallsThenReportsNoChange(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "brw")
	writeTree(t, source, map[string]string{
		"SKILL.md":              "# brw\n",
		"references/recipes.md": "recipes\n",
	})

	changed, err := CopyTree(source, destination)
	if err != nil || !changed {
		t.Fatalf("first install changed=%v err=%v", changed, err)
	}
	if got := readFile(t, filepath.Join(destination, "references", "recipes.md")); got != "recipes\n" {
		t.Fatalf("nested file = %q", got)
	}

	changed, err = CopyTree(source, destination)
	if err != nil || changed {
		t.Fatalf("re-install must be a no-op, changed=%v err=%v", changed, err)
	}

	// A page the source dropped must not survive as instructions an agent
	// still reads.
	if err := os.WriteFile(filepath.Join(destination, "stale.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = CopyTree(source, destination)
	if err != nil || !changed {
		t.Fatalf("stale removal changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "stale.md")); !os.IsNotExist(err) {
		t.Fatalf("stale file survived: %v", err)
	}

	// A locally edited page is restored from the source.
	if err := os.WriteFile(filepath.Join(destination, "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := CopyTree(source, destination); err != nil || !changed {
		t.Fatalf("edited file restore changed=%v err=%v", changed, err)
	}
	if got := readFile(t, filepath.Join(destination, "SKILL.md")); got != "# brw\n" {
		t.Fatalf("SKILL.md = %q, want the source content", got)
	}
}

func TestFindSkillSource(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	workingDir := filepath.Join(root, "checkout")
	writeTree(t, filepath.Join(appDir, "skills", "brw"), map[string]string{"SKILL.md": "app\n"})
	writeTree(t, filepath.Join(workingDir, "skills", "brw"), map[string]string{"SKILL.md": "checkout\n"})

	cases := []struct {
		name       string
		appDir     string
		workingDir string
		want       string
		wantErr    bool
	}{
		{name: "app dir wins", appDir: appDir, workingDir: workingDir, want: filepath.Join(appDir, "skills", "brw")},
		{name: "falls back to the checkout", appDir: filepath.Join(root, "absent"), workingDir: workingDir, want: filepath.Join(workingDir, "skills", "brw")},
		{name: "nothing anywhere", appDir: filepath.Join(root, "absent"), workingDir: filepath.Join(root, "absent"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindSkillSource(tc.appDir, "", tc.workingDir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("FindSkillSource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkillDestinations(t *testing.T) {
	got := SkillDestinations("/home/someone")
	want := []string{
		"/home/someone/.claude/skills/brw",
		"/home/someone/.agents/skills/brw",
		"/home/someone/.codex/skills/brw",
	}
	if len(got) != len(want) {
		t.Fatalf("destinations = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("destination %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestProfileDirectories covers the first-launch case that produced a policy
// doctor could not verify: a browser that is installed but has never created a
// profile directory.
func TestProfileDirectories(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    []string
		wantOne string
	}{
		{
			name:    "never launched",
			files:   map[string]string{"Local State": "{}"},
			want:    nil,
			wantOne: "Default",
		},
		{
			name: "default and numbered profiles order",
			files: map[string]string{
				"Default/Preferences":    "{}",
				"Profile 10/Preferences": "{}",
				"Profile 2/Preferences":  "{}",
			},
			want:    []string{"Default", "Profile 2", "Profile 10"},
			wantOne: "Default",
		},
		{
			name:    "numbered only",
			files:   map[string]string{"Profile 1/Preferences": "{}"},
			want:    []string{"Profile 1"},
			wantOne: "Profile 1",
		},
		{
			name: "a directory without Preferences is not a profile",
			files: map[string]string{
				"ShaderCache/index":     "{}",
				"Profile 1/Preferences": "{}",
			},
			want:    []string{"Profile 1"},
			wantOne: "Profile 1",
		},
		{
			// Chrome writes Preferences into these, but nobody signs into them.
			name: "chrome scaffolding profiles are excluded",
			files: map[string]string{
				"System Profile/Preferences": "{}",
				"Guest Profile/Preferences":  "{}",
				"Profile 2/Preferences":      "{}",
			},
			want:    []string{"Profile 2"},
			wantOne: "Profile 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)
			got := ProfileDirectories(dir)
			if len(got) != len(tc.want) {
				t.Fatalf("ProfileDirectories = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ProfileDirectories = %v, want %v", got, tc.want)
				}
			}
			if picked := PickProfileDirectory(dir); picked != tc.wantOne {
				t.Fatalf("PickProfileDirectory = %q, want %q", picked, tc.wantOne)
			}
			if HasProfiles(dir) != (len(tc.want) > 0) {
				t.Fatalf("HasProfiles = %v", HasProfiles(dir))
			}
		})
	}
	if HasProfiles(filepath.Join(t.TempDir(), "absent")) {
		t.Fatal("a missing user data directory must not report profiles")
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
