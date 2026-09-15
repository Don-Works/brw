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

// callSkillTool runs brw_skill against a server with no controller. The tool is
// in tabAgnosticTools and touches no browser, so a nil manager is the honest
// fixture: anything that reached for one would panic here rather than pass.
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

// TestServedSkillCarriesTheVersionOfTheBinaryServingIt is the whole point of
// serving the skill from the daemon: the page and the tool surface it describes
// come out of one build, and the page says which.
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
	// The same call carries the version brw_identity reports, so an agent can
	// line a manual up against the daemon that gave it. A version reported by
	// only one of the two is a version nobody can check.
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

// TestStaleSkillOnDiskDoesNotShadowTheServedSkill is the failure this feature
// exists to remove. `brwctl setup` writes skills/brw onto disk; upgrade the
// daemon without re-running setup and that copy describes a surface this build
// no longer has, with nothing to say so.
//
// The decoy is planted at every path the old on-disk lookup searched — the
// working directory, and the directory the executable sits in — so the test
// fails if the serving path is ever changed to read a file.
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
		// The test binary's own directory is a temp dir the go tool owns, so
		// writing a decoy beside it is safe and is exactly where the old
		// "next to the install" lookup would have found one.
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

// jsonString quotes a string as JSON. Named for what it does at the call site;
// json.Marshal on a string cannot fail.
func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TestSkillToolRefusesADocumentOutsideTheEmbeddedSet keeps the document
// argument from becoming a file reader. The guard is membership in what was
// embedded, so a path that escapes a prefix check still has nothing to resolve
// against.
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

// TestEverySkillDocumentIsServable enumerates the embedded set rather than
// naming pages by hand: a reference added to skills/brw that the serving path
// cannot return is a page an agent is told about and then refused.
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
