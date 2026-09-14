package mcp

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// skillToolMention matches a tool name wherever the skill file names one, in
// prose or in a table cell. The trailing class has to admit digits or
// brw_a11y_audit reads as brw_a.
var skillToolMention = regexp.MustCompile(`brw_[a-z0-9_]+`)

// skillToolCall matches only a call-shaped mention. The reverse check uses it
// because the skill file also names DOM ids and profile directories that share
// the prefix, and those are prose, not a claim that a tool exists.
var skillToolCall = regexp.MustCompile(`brw_[a-z0-9_]+\(`)

// TestEverySkillDocumentsEveryAdvertisedTool: skills/brw/SKILL.md is the
// agent-facing surface doc, it ships in the package (see
// packaging/package_contents_test.go and internal/setup/skills.go), and a tool
// that is in tools/list but not in the skill is one an agent has to discover by
// accident. Enumerating the catalogue rather than listing names by hand is the
// point: a tool added next week fails this test on the commit that adds it.
func TestEverySkillDocumentsEveryAdvertisedTool(t *testing.T) {
	skill, err := os.ReadFile("../../skills/brw/SKILL.md")
	if err != nil {
		t.Fatalf("read the brw skill: %v", err)
	}
	documented := map[string]bool{}
	for _, mention := range skillToolMention.FindAllString(string(skill), -1) {
		documented[mention] = true
	}

	advertised := (&Server{toolProfile: "all"}).advertisedTools()
	if len(advertised) == 0 {
		t.Fatal("the full catalogue is empty, so this test would pass by vacuum")
	}
	for _, tl := range advertised {
		name, _ := tl["name"].(string)
		if name == "" {
			t.Fatalf("a catalogue entry has no name: %v", tl)
		}
		if !documented[name] {
			t.Errorf("%s is advertised in tools/list but never mentioned in skills/brw/SKILL.md", name)
		}
	}

	// The reverse direction catches the other drift: a skill entry for a tool
	// that was renamed or removed sends an agent after something that no longer
	// answers.
	for _, call := range skillToolCall.FindAllString(string(skill), -1) {
		name := strings.TrimSuffix(call, "(")
		if !catalogueHasTool(advertised, name) {
			t.Errorf("skills/brw/SKILL.md documents %s({...}), which is not in the catalogue", name)
		}
	}
}

func catalogueHasTool(catalogue []map[string]any, name string) bool {
	for _, tl := range catalogue {
		if got, _ := tl["name"].(string); got == name {
			return true
		}
	}
	return false
}
