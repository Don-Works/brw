package browsertest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The eleventh test to hand t.TempDir() straight to a browser would go red on
// Linux CI and nowhere else, so nothing anyone runs before pushing would catch
// it. This catches it here instead, on every platform, in a scan with no
// browser in it.
//
// Two spellings name a browser profile directly enough to be unambiguous: the
// UserDataDir field of a brw or chromedp launch config, and chromedp's option
// of the same name. A directory reached some other way is not this test's
// business — the reclaim is what makes it safe, not the spelling.
var profileFromTempDir = regexp.MustCompile(`UserDataDir:\s*t\.TempDir\(\)|chromedp\.UserDataDir\(t\.TempDir\(\)\)`)

func TestNoTestHandsATempDirStraightToABrowser(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "bin", "store-assets":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") || path == selfPath(t) {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		for number, line := range strings.Split(string(source), "\n") {
			if profileFromTempDir.MatchString(line) {
				found = append(found, fmt.Sprintf("%s:%d", relative, number+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("%s hand t.TempDir() to a browser as its profile directory.\n"+
			"testing's TempDir cleanup is a single strict RemoveAll, and on Linux it races the last Chrome helper still writing the profile — the test then fails on the cleanup rather than on anything it asserts.\n"+
			"Take the directory from browsertest.NewProfile(t) and register the browser's shutdown with StopWith.",
			strings.Join(found, ", "))
	}
}

// selfPath is this file, which holds both spellings inside a regexp literal.
func selfPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("guard_test.go")
	if err != nil {
		t.Fatalf("locate this file: %v", err)
	}
	return path
}
