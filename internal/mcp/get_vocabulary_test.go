package mcp

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// vocabularyRun matches a pipe-separated list of lowercase words, the shape
// every hand-written copy of the brw_get vocabulary takes.
var vocabularyRun = regexp.MustCompile(`[a-z_]+(?:\|[a-z_]+){3,}`)

// TestGetVocabularyIsSpelledOutOnceEverywhere: brw_get's enum is derived from
// snapshot.GetKindNames, but agents read the prose beside it, and the skill file
// is the only description a human ever sees. Two hand-kept copies of the same
// list are two chances to advertise a kind Validate refuses, or to hide one it
// accepts — and the drift is invisible until an agent asks for the missing kind.
func TestGetVocabularyIsSpelledOutOnceEverywhere(t *testing.T) {
	want := strings.Join(snapshot.GetKindNames(), "|")

	skill, err := os.ReadFile("../../skills/brw/SKILL.md")
	if err != nil {
		t.Fatalf("read the brw skill: %v", err)
	}
	description, _ := toolByName(t, "brw_get")["description"].(string)
	if description == "" {
		t.Fatal("brw_get has no description")
	}

	for _, source := range []struct {
		name string
		text string
	}{
		{name: "the brw_get tool description", text: description},
		{name: "skills/brw/SKILL.md", text: string(skill)},
	} {
		t.Run(source.name, func(t *testing.T) {
			runs := vocabularyRun.FindAllString(source.text, -1)
			if len(runs) == 0 {
				t.Fatalf("%s spells out no vocabulary at all, so a reader has only the enum", source.name)
			}
			for _, run := range runs {
				if run != want {
					t.Errorf("%s names %q, want %q", source.name, run, want)
				}
			}
		})
	}
}
