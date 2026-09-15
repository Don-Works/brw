package harness

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixturesServeOnlyWhatIsInsideTheRoot enumerates the ways a request can
// name a file outside the fixture directory. The interesting member is the
// symlink: a check written against the requested string passes it, because the
// string contains no "..", and only a check on the resolved path refuses it.
func TestFixturesServeOnlyWhatIsInsideTheRoot(t *testing.T) {
	repoRoot := t.TempDir()
	fixtureDir := filepath.Join(repoRoot, "tests", "fixtures")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "page.html"), []byte("<h1>inside</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(repoRoot, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fixtureDir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(repoRoot, filepath.Join(fixtureDir, "up")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fixtures, err := ServeFixtures(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer fixtures.Close()

	cases := []struct {
		name    string
		path    string
		wantOK  bool
		wantHas string
	}{
		{name: "regular file", path: "/page.html", wantOK: true, wantHas: "inside"},
		{name: "dot dot traversal", path: "/../outside.txt"},
		{name: "encoded traversal", path: "/%2e%2e/outside.txt"},
		{name: "nested traversal", path: "/sub/../../outside.txt"},
		{name: "symlink to a file outside", path: "/link.txt"},
		{name: "symlink to a directory outside", path: "/up/outside.txt"},
		{name: "directory listing", path: "/"},
		{name: "missing file", path: "/nope.html"},
	}

	client := &http.Client{}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resp, err := client.Get(fixtures.BaseURL() + testCase.path)
			if err != nil {
				t.Fatalf("get %s: %v", testCase.path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if testCase.wantOK {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %s, want 200", resp.Status)
				}
				if !strings.Contains(string(body), testCase.wantHas) {
					t.Fatalf("body %q missing %q", body, testCase.wantHas)
				}
				return
			}
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s was served: status 200, body %q", testCase.path, body)
			}
			if strings.Contains(string(body), "OUTSIDE") {
				t.Fatalf("%s leaked content from outside the fixture root", testCase.path)
			}
		})
	}
}

// TestFixtureDigestTracksContent is what makes the fingerprint load-bearing:
// two runs against different fixture bytes must not claim to be comparable.
func TestFixtureDigestTracksContent(t *testing.T) {
	repoRoot := t.TempDir()
	fixtureDir := filepath.Join(repoRoot, "tests", "fixtures")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(fixtureDir, "page.html")
	if err := os.WriteFile(page, []byte("<h1>one</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	fixtures, err := ServeFixtures(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer fixtures.Close()

	first, err := fixtures.Digest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := fixtures.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("digest is not stable: %s then %s", first, again)
	}

	if err := os.WriteFile(page, []byte("<h1>two</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := fixtures.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("digest did not change after a fixture changed")
	}

	if err := os.WriteFile(filepath.Join(fixtureDir, "extra.html"), []byte("<h1>two</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	withExtra, err := fixtures.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if withExtra == changed {
		t.Fatal("digest did not change after a fixture was added")
	}
}
