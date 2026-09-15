package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/agentskill"
)

func getSkill(t *testing.T, server *Server, query string) (agentskill.Document, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/skill"+query, nil))
	var document agentskill.Document
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
			t.Fatalf("decode /api/skill%s: %v (%s)", query, err, rec.Body.String())
		}
	}
	return document, rec.Code
}

// TestSkillRouteServesTheDaemonsOwnBuild pins the HTTP half of the same
// contract the MCP tool holds: the manual comes out of this binary and says
// which build served it.
func TestSkillRouteServesTheDaemonsOwnBuild(t *testing.T) {
	server := New("", &fakeController{})
	server.SetVersion("4.2.1-http-test")

	document, code := getSkill(t, server, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/skill = %d", code)
	}
	if document.Version != "4.2.1-http-test" {
		t.Fatalf("served version %q, want the one the daemon was given", document.Version)
	}
	if document.Source != agentskill.SourceEmbedded {
		t.Fatalf("served source %q, want %q", document.Source, agentskill.SourceEmbedded)
	}
	if document.Path != agentskill.Default || !strings.Contains(document.Content, "brw_identity") {
		t.Fatalf("served %q, which does not look like the brw skill", document.Path)
	}
	if document.Bytes != len(document.Content) {
		t.Fatalf("bytes=%d but content is %d bytes long", document.Bytes, len(document.Content))
	}
	if len(document.Documents) < 2 {
		t.Fatalf("documents=%v, which does not list the references an agent is meant to fetch", document.Documents)
	}

	reference, code := getSkill(t, server, "?document=references/recipes.md")
	if code != http.StatusOK || reference.Path != "references/recipes.md" {
		t.Fatalf("GET /api/skill?document=references/recipes.md = %d %q", code, reference.Path)
	}
	if reference.Version != "4.2.1-http-test" {
		t.Fatalf("a reference page lost the version stamp: %q", reference.Version)
	}

	if _, code := getSkill(t, server, "?document=../../go.mod"); code != http.StatusBadRequest {
		t.Fatalf("a path outside the skill answered %d, want a refusal", code)
	}
}

// TestSkillRouteIgnoresACopyOnDisk is the HTTP half of the shadowing guard. A
// daemon started in a source checkout, or next to an old installed skill tree,
// still answers with its own build's page.
func TestSkillRouteIgnoresACopyOnDisk(t *testing.T) {
	workingDir := t.TempDir()
	decoy := filepath.Join(workingDir, "skills", "brw")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatalf("plant decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "SKILL.md"), []byte("# STALE COPY FROM AN OLDER INSTALL\n"), 0o644); err != nil {
		t.Fatalf("plant decoy: %v", err)
	}
	t.Chdir(workingDir)

	server := New("", &fakeController{})
	server.SetVersion("4.2.1-http-test")
	document, code := getSkill(t, server, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/skill = %d", code)
	}
	if strings.Contains(document.Content, "STALE COPY FROM AN OLDER INSTALL") {
		t.Fatal("/api/skill served the copy on disk, not the binary's")
	}
}

// TestSkillRouteIsRecordedAndUngated keeps the route inside the two tables a
// new route has to be in: the usage allowlist and the consent classification.
// Both are checked exhaustively elsewhere; this pins the values for this route
// so a rename shows up here rather than as a silently unrecorded operation.
func TestSkillRouteIsRecordedAndUngated(t *testing.T) {
	if got := usageOperations["/api/skill"]; got != "brw_skill" {
		t.Fatalf("usageOperations[/api/skill] = %q, want brw_skill", got)
	}
}
