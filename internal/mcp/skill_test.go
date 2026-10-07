package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/agentskill"
)

func callSkillTool(t *testing.T, args string) (map[string]any, bool) {
	t.Helper()
	server := &Server{toolProfile: "all"}
	result, rpcErr := server.callTool(context.Background(), skillToolName, json.RawMessage(args))
	if rpcErr != nil {
		t.Fatalf("callTool(%s): %+v", skillToolName, rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var envelope struct {
		IsError           bool           `json:"isError"`
		StructuredContent map[string]any `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.IsError {
		text := ""
		if len(envelope.Content) > 0 {
			text = envelope.Content[0].Text
		}
		return map[string]any{"error": text}, false
	}
	return envelope.StructuredContent, true
}

func TestServedSkillCarriesTheVersionOfTheBinaryServingIt(t *testing.T) {
	const stamped = "9.9.9-test-build"
	previous := Version
	Version = stamped
	t.Cleanup(func() { Version = previous })

	document, ok := callSkillTool(t, `{}`)
	if !ok {
		t.Fatalf("brw_skill refused: %v", document)
	}
	if got, _ := document["version"].(string); got != stamped {
		t.Fatalf("served skill reports version %q, want the binary's %q", got, stamped)
	}
	if got, _ := document["path"].(string); got != agentskill.Default {
		t.Fatalf("no document named: got path %q, want %q", got, agentskill.Default)
	}
	if got, _ := document["source"].(string); got != agentskill.SourceEmbedded {
		t.Fatalf("served skill reports source %q, want %q", got, agentskill.SourceEmbedded)
	}
	content, _ := document["content"].(string)
	if !strings.Contains(content, "# brw — driving a real browser over MCP") {
		t.Fatalf("served skill is not the brw skill page:\n%.400s", content)
	}

	identity, rpcErr := (&Server{toolProfile: "all"}).callTool(context.Background(), "brw_identity", json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("brw_identity: %+v", rpcErr)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	if !strings.Contains(string(encoded), stamped) {
		t.Fatalf("brw_identity does not report %q, so the two surfaces disagree: %s", stamped, encoded)
	}
}

func TestStaleSkillOnDiskDoesNotShadowTheServedSkill(t *testing.T) {
	const stale = "---\nname: brw\n---\n\n# STALE COPY FROM AN OLDER INSTALL\n"

	workingDir := t.TempDir()
	plant := func(base string) {
		dir := filepath.Join(base, "skills", "brw")
		if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
			t.Fatalf("plant decoy: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(stale), 0o644); err != nil {
			t.Fatalf("plant decoy: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "references", "recipes.md"), []byte(stale), 0o644); err != nil {
			t.Fatalf("plant decoy: %v", err)
		}
	}
	plant(workingDir)
	executable, err := os.Executable()
	if err == nil {

		plant(filepath.Dir(executable))
		plant(filepath.Join(filepath.Dir(executable), ".."))
	}
	t.Chdir(workingDir)

	for _, document := range []string{"", "SKILL.md", "references/recipes.md"} {
		served, ok := callSkillTool(t, `{"document":`+jsonString(document)+`}`)
		if !ok {
			t.Fatalf("brw_skill(%q) refused: %v", document, served)
		}
		content, _ := served["content"].(string)
		if strings.Contains(content, "STALE COPY FROM AN OLDER INSTALL") {
			t.Fatalf("brw_skill(%q) served the copy on disk, not the binary's", document)
		}
		if got, _ := served["source"].(string); got != agentskill.SourceEmbedded {
			t.Fatalf("brw_skill(%q) reports source %q", document, got)
		}
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestSkillToolRefusesADocumentOutsideTheEmbeddedSet(t *testing.T) {
	escapes := []string{
		"../../../etc/hosts",
		"references/../../go.mod",
		"/etc/hosts",
		"references",
		"SKILL.md/../SKILL.md",
	}
	for _, name := range escapes {
		result, ok := callSkillTool(t, `{"document":`+jsonString(name)+`}`)
		if ok {
			t.Errorf("brw_skill served %q, which is not a document of the skill", name)
			continue
		}
		message, _ := result["error"].(string)
		if !strings.Contains(message, agentskill.Default) {
			t.Errorf("the refusal for %q does not name the real documents: %q", name, message)
		}
	}
}

func TestEverySkillDocumentIsServable(t *testing.T) {
	all, err := agentskill.Documents()
	if err != nil {
		t.Fatalf("list the embedded skill: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("the embedded skill has %d documents; this test would pass by vacuum", len(all))
	}
	for _, name := range all {
		served, ok := callSkillTool(t, `{"document":`+jsonString(name)+`}`)
		if !ok {
			t.Errorf("the skill lists %q but brw_skill cannot serve it: %v", name, served)
			continue
		}
		if content, _ := served["content"].(string); strings.TrimSpace(content) == "" {
			t.Errorf("brw_skill served %q with no content", name)
		}
	}
}
