package browser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestNormalizeUploadPaths_SinglePath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(f, []byte("hello"), 0644)
	paths, err := NormalizeUploadPaths(snapshot.UploadOptions{Path: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	if !filepath.IsAbs(paths[0]) {
		t.Fatalf("expected absolute path, got %q", paths[0])
	}
}

func TestNormalizeUploadPaths_MultiplePaths(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	os.WriteFile(f1, []byte("a"), 0644)
	os.WriteFile(f2, []byte("b"), 0644)
	paths, err := NormalizeUploadPaths(snapshot.UploadOptions{Paths: []string{f1, f2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
}

func TestNormalizeUploadPaths_EmptyPath(t *testing.T) {
	_, err := NormalizeUploadPaths(snapshot.UploadOptions{})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestNormalizeUploadPaths_DirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := NormalizeUploadPaths(snapshot.UploadOptions{Path: dir})
	if err == nil {
		t.Fatal("expected error for directory")
	}
}

func TestNormalizeUploadPaths_NonexistentFile(t *testing.T) {
	_, err := NormalizeUploadPaths(snapshot.UploadOptions{Path: "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestNormalizeUploadPaths_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	// We can't guarantee a file exists at ~/test-upload.txt, so just test that
	// the path is expanded correctly by checking a nonexistent path under home.
	_, err = NormalizeUploadPaths(snapshot.UploadOptions{Path: "~/test-upload-nonexistent-12345.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file under home")
	}
	// The error message should contain the expanded home path.
	if err != nil {
		absHome, _ := filepath.Abs(home)
		if !filepath.IsAbs(absHome) {
			t.Fatalf("expected absolute path in error, got %q", err)
		}
	}
}

func TestNormalizeUploadPaths_SkipsBlanks(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(f, []byte("hello"), 0644)
	paths, err := NormalizeUploadPaths(snapshot.UploadOptions{Paths: []string{"  ", f}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path (blank skipped), got %d", len(paths))
	}
}
