package mcp

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

var vocabularyRun = regexp.MustCompile(`[a-z_]+(?:\|[a-z_]+){3,}`)

func TestGetVocabularyIsSpelledOutOnceEverywhere(t *testing.T) {
	want := strings.Join(snapshot.GetKindNames(), "|")

	skill, err := os.ReadFile("../../skills/brw/references/tool-catalogue.md")
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
