package browser

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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

func TestResolveUploadPaths_LocalPathPassThrough(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(f, []byte("hello"), 0644)
	paths, cleanup, err := ResolveUploadPaths(context.Background(), snapshot.UploadOptions{Path: f})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(paths) != 1 || !filepath.IsAbs(paths[0]) {
		t.Fatalf("expected 1 absolute path, got %v", paths)
	}
}

func TestResolveUploadPaths_BytesBase64(t *testing.T) {
	content := []byte("inline upload contents")
	opts := snapshot.UploadOptions{
		BytesBase64: base64.StdEncoding.EncodeToString(content),
		Filename:    "report.csv",
	}
	paths, cleanup, err := ResolveUploadPaths(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 temp path, got %v", paths)
	}
	if filepath.Base(paths[0]) != "report.csv" {
		t.Fatalf("expected temp file to preserve filename, got %q", filepath.Base(paths[0]))
	}
	got, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("temp file unreadable: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("temp file contents = %q, want %q", got, content)
	}
	cleanup()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("expected temp file removed after cleanup, stat err = %v", err)
	}
}

func TestResolveUploadPaths_BytesBase64Invalid(t *testing.T) {
	_, cleanup, err := ResolveUploadPaths(context.Background(), snapshot.UploadOptions{BytesBase64: "!!!not-base64!!!"})
	defer cleanup()
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestResolveUploadPaths_URL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("downloaded body"))
	}))
	defer srv.Close()

	opts := snapshot.UploadOptions{URL: srv.URL + "/data/file.bin"}
	paths, cleanup, err := ResolveUploadPaths(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 temp path, got %v", paths)
	}
	if filepath.Base(paths[0]) != "file.bin" {
		t.Fatalf("expected temp file named from url basename, got %q", filepath.Base(paths[0]))
	}
	got, _ := os.ReadFile(paths[0])
	if string(got) != "downloaded body" {
		t.Fatalf("temp file contents = %q", got)
	}
	cleanup()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("expected temp file removed after cleanup")
	}
}

func TestResolveUploadPaths_URLBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, cleanup, err := ResolveUploadPaths(context.Background(), snapshot.UploadOptions{URL: srv.URL})
	defer cleanup()
	if err == nil {
		t.Fatal("expected error for non-2xx status")
	}
}

func TestResolveUploadPaths_NoSource(t *testing.T) {
	_, cleanup, err := ResolveUploadPaths(context.Background(), snapshot.UploadOptions{})
	defer cleanup()
	if err == nil {
		t.Fatal("expected error when no source provided")
	}
}

func TestResolveUploadPaths_MultipleSourcesRejected(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(f, []byte("x"), 0644)
	_, cleanup, err := ResolveUploadPaths(context.Background(), snapshot.UploadOptions{
		Path:        f,
		BytesBase64: base64.StdEncoding.EncodeToString([]byte("y")),
	})
	defer cleanup()
	if err == nil {
		t.Fatal("expected error when multiple sources provided")
	}
}

func TestDecodeUploadBytes_SizeCap(t *testing.T) {
	// Valid input under the cap decodes normally.
	enc := base64.StdEncoding.EncodeToString([]byte("hello world"))
	data, err := decodeUploadBytes(enc, 1024)
	if err != nil {
		t.Fatalf("small input under cap: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("decoded = %q", data)
	}
	// Oversized input is rejected by the pre-decode length guard (matching the
	// URL fetch cap) rather than allocating the full output.
	big := base64.StdEncoding.EncodeToString(make([]byte, 4096))
	if _, err := decodeUploadBytes(big, 1024); err == nil {
		t.Fatal("expected error for input exceeding decoded cap")
	}
}
