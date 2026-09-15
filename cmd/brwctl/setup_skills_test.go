package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/agentskill"
	"github.com/Don-Works/brw/internal/setup"
)

// TestSetupInstallsTheSkillFromThisBinary is the install half of the
// no-shadowing rule. Setup used to look for a skills/brw directory next to the
// executable or in the working directory, so whichever brw happened to run
// setup decided what every later agent read — and an old tree left behind by a
// previous install won that search.
func TestSetupInstallsTheSkillFromThisBinary(t *testing.T) {
	home := t.TempDir()
	workingDir := t.TempDir()
	appDir := filepath.Join(workingDir, "app")

	// Decoys at both places the old lookup searched.
	for _, base := range []string{workingDir, appDir} {
		decoy := filepath.Join(base, "skills", "brw")
		if err := os.MkdirAll(decoy, 0o755); err != nil {
			t.Fatalf("plant decoy: %v", err)
		}
		if err := os.WriteFile(filepath.Join(decoy, "SKILL.md"), []byte("# STALE COPY FROM AN OLDER INSTALL\n"), 0o644); err != nil {
			t.Fatalf("plant decoy: %v", err)
		}
	}

	runner := &setupRunner{opts: setupOptions{
		home: home, workingDir: workingDir, appDir: appDir, out: &bytes.Buffer{},
	}}
	runner.stepSkills()

	embedded, err := agentskill.Read(agentskill.Default, "irrelevant")
	if err != nil {
		t.Fatalf("read the embedded skill: %v", err)
	}
	destinations := setup.SkillDestinations(home)
	if len(destinations) == 0 {
		t.Fatal("no skill destinations, so this test would pass by vacuum")
	}
	for _, destination := range destinations {
		installed, err := os.ReadFile(filepath.Join(destination, "SKILL.md"))
		if err != nil {
			t.Fatalf("read the installed skill at %s: %v", destination, err)
		}
		if strings.Contains(string(installed), "STALE COPY FROM AN OLDER INSTALL") {
			t.Fatalf("%s got the copy on disk, not the binary's", destination)
		}
		if string(installed) != embedded.Content {
			t.Fatalf("%s does not match the binary's copy", destination)
		}
		// The references travel with it: a SKILL.md that links to a page the
		// install never wrote is a dead link in the agent's manual.
		if _, err := os.Stat(filepath.Join(destination, "references", "recipes.md")); err != nil {
			t.Fatalf("reference page missing at %s: %v", destination, err)
		}
	}
}

// TestSetupSkillsDirOverrideStillWorks keeps the escape hatch honest: someone
// editing the skill itself must still be able to install what they are editing.
func TestSetupSkillsDirOverrideStillWorks(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "skills", "brw")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("# work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &setupRunner{opts: setupOptions{home: home, skillsDir: source, out: &bytes.Buffer{}}}
	runner.stepSkills()

	installed, err := os.ReadFile(filepath.Join(setup.SkillDestinations(home)[0], "SKILL.md"))
	if err != nil {
		t.Fatalf("read the installed skill: %v", err)
	}
	if string(installed) != "# work in progress\n" {
		t.Fatalf("--skills-dir did not win: %q", installed)
	}
}
