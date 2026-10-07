package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

func TestFixtureServerRetainsStartupFailure(t *testing.T) {
	r := &runner{repoRoot: t.TempDir()}
	for range 2 {
		if base, err := r.httpFixturesBase(); err == nil || base != "" {
			t.Fatalf("missing fixtures accepted: %q %v", base, err)
		}
	}
}

func TestFixtureServerRefusesFilesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	fixtures := filepath.Join(root, "tests", "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fixtures, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	r := &runner{repoRoot: root}
	base, err := r.httpFixturesBase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.httpFixtures.Close() })
	resp, err := http.Get(base + "/escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outside file served: %s", resp.Status)
	}
}

func TestEnvironmentExpansionTreatsValuesAsData(t *testing.T) {
	t.Setenv("BRWCHECK_VALUE", "${ENV:BRWCHECK_OTHER}")
	t.Setenv("BRWCHECK_OTHER", "expanded")
	if got := expandVars("prefix ${ENV:BRWCHECK_VALUE} suffix"); got != "prefix ${ENV:BRWCHECK_OTHER} suffix" {
		t.Fatalf("value recursively interpreted: %q", got)
	}
	t.Setenv("BRWCHECK_VALUE", "${ENV:BRWCHECK_VALUE}")
	if got := expandVars("${ENV:BRWCHECK_VALUE}"); got != "${ENV:BRWCHECK_VALUE}" {
		t.Fatalf("self-reference changed: %q", got)
	}
	t.Setenv("BRWCHECK_MISSING", "")
	if got := expandVars("${FIXTURES}/$literal/${ENV:BRWCHECK_MISSING:https://fallback.test}/${ENV:BRWCHECK_OTHER}/${ENV:unfinished"); got != "${FIXTURES}/$literal/https://fallback.test/expanded/${ENV:unfinished" {
		t.Fatalf("expansion=%q", got)
	}
}

func TestAPIClientRejectsFailedActionResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"message":"action refused"}`))
	}))
	defer server.Close()
	client := &apiClient{base: server.URL, http: server.Client()}
	var result browser.ActionResult
	if err := client.postJSON("/action", nil, &result); err == nil {
		t.Fatal("failed action accepted")
	}
}

func TestBatchRejectsTopLevelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"error":"batch refused","steps":[]}`))
	}))
	defer server.Close()
	r := &runner{client: &apiClient{base: server.URL, http: server.Client()}}
	if err := r.runBatchStep(batchStep{}); err == nil {
		t.Fatal("failed batch accepted")
	}
}
