package agentskill

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEmbeddedSkillMatchesTheCommittedTree is the guard on the embed itself.
// An embed pattern that stops matching — a reference renamed, a new
// subdirectory — fails silently: the binary keeps building and serves a skill
// missing the page it points at.
func TestEmbeddedSkillMatchesTheCommittedTree(t *testing.T) {
	var onDisk []string
	root := filepath.Join("..", "..", "skills", "brw")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		onDisk = append(onDisk, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(onDisk) < 2 {
		t.Fatalf("found %d markdown files under %s; this test would pass by vacuum", len(onDisk), root)
	}

	embedded, err := Documents()
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	for _, name := range onDisk {
		if !slices.Contains(embedded, name) {
			t.Errorf("%s is committed under skills/brw but the binary does not carry it; the embed pattern stopped matching it", name)
		}
	}
	for _, name := range embedded {
		if !slices.Contains(onDisk, name) {
			t.Errorf("the binary carries %s, which is not a committed markdown file", name)
		}
	}

	// And the bytes are the committed bytes, not an older build's.
	for _, name := range embedded {
		want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got, err := Read(name, "test")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		if got.Content != string(want) {
			t.Errorf("the embedded %s differs from the committed file", name)
		}
		if got.Bytes != len(want) {
			t.Errorf("%s reports %d bytes, is %d", name, got.Bytes, len(want))
		}
	}
}

// TestSKILLmdIsFirst: an agent that fetches without naming a document must get
// the page that tells it about the others.
func TestSKILLmdIsFirst(t *testing.T) {
	documents, err := Documents()
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if documents[0] != Default {
		t.Fatalf("documents[0] = %q, want %q", documents[0], Default)
	}
	empty, err := Read("", "v1")
	if err != nil {
		t.Fatalf("Read(\"\"): %v", err)
	}
	if empty.Path != Default {
		t.Fatalf("an unnamed document resolved to %q", empty.Path)
	}
}

// TestReadIsBoundedToTheEmbeddedSet. The allowlist is what was embedded, which
// is a property of the binary; a prefix or ".." check on the request would be a
// property of the string somebody sent.
func TestReadIsBoundedToTheEmbeddedSet(t *testing.T) {
	for _, name := range []string{
		"../../go.mod",
		"/etc/hosts",
		"references/../../../etc/hosts",
		"references",
		"",  // handled: this one is the default, checked below
		" ", // whitespace is not a document either
	} {
		document, err := Read(name, "v1")
		if strings.TrimSpace(name) == "" {
			if err != nil || document.Path != Default {
				t.Errorf("Read(%q) should be the default document: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Read(%q) returned %q", name, document.Path)
		}
	}
}

// TestFSCarriesTheWholeTree: the installer writes what this returns, so a
// reference missing from it is a dead link in the manual on disk.
func TestFSCarriesTheWholeTree(t *testing.T) {
	tree, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	documents, err := Documents()
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	for _, name := range documents {
		data, err := fs.ReadFile(tree, name)
		if err != nil {
			t.Errorf("the installable tree has no %s: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty in the installable tree", name)
		}
	}
}
