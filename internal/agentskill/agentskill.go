// Package agentskill serves the brw agent skill out of the running daemon.
//
// An agent that reads the skill off disk reads whatever `brwctl setup` last
// copied there, which is the version of brw the machine had when setup ran, not
// the version of brwd it is now talking to. A daemon that has been upgraded
// under a stale on-disk copy then advertises tools the page does not mention
// and documents arguments the build no longer takes, and nothing says so.
//
// Serving the skill from the binary removes the question: the document a daemon
// returns came out of that daemon's own build, and it is stamped with that
// build's version. Disk is never consulted on this path, so no copy on disk can
// shadow it.
package agentskill

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/Don-Works/brw/skills"
)

// Name is the skill's own name, matching skills/brw and the directory
// `brwctl setup` installs into.
const Name = "brw"

// Default is the document a caller gets when it names none.
const Default = "SKILL.md"

// embedRoot is the directory inside the embedded FS that holds the skill.
const embedRoot = "brw"

// Document is one served skill page.
type Document struct {
	// Skill is always Name. It is in the payload so a caller holding several
	// skills' pages can tell them apart without tracking where each came from.
	Skill string `json:"skill"`
	// Path is the document's path within the skill, e.g. "SKILL.md" or
	// "references/recipes.md".
	Path string `json:"path"`
	// Version is the version of the daemon that served this document. It is the
	// point of the whole package: the page and the tool surface it describes
	// come from one build.
	Version string `json:"version"`
	// Source names where the bytes came from, so an agent comparing this with a
	// copy on disk knows which one is authoritative.
	Source string `json:"source"`
	// Documents lists every page this skill has, so a caller can fetch a
	// reference without guessing its path.
	Documents []string `json:"documents"`
	// Bytes is the document's length, which lets a caller decide whether to read
	// it now or on demand without counting the content itself.
	Bytes   int    `json:"bytes"`
	Content string `json:"content"`
}

// SourceEmbedded is the only Source value brw ever serves. It is a constant
// rather than a free-form string so a caller can branch on it.
const SourceEmbedded = "brwd-binary"

var (
	once      sync.Once
	documents []string
	indexErr  error
)

// index walks the embedded tree once and keeps the resulting path set. The set
// is also the allowlist Read checks against: membership in what was actually
// embedded is a property of the bytes in the binary, where a prefix or ".."
// check on the requested string is a property of the request, and a request can
// be spelled to defeat it.
func index() ([]string, error) {
	once.Do(func() {
		err := fs.WalkDir(skills.FS, embedRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			relative, err := relativeTo(embedRoot, p)
			if err != nil {
				return err
			}
			documents = append(documents, relative)
			return nil
		})
		if err != nil {
			indexErr = fmt.Errorf("index the embedded brw skill: %w", err)
			return
		}
		if len(documents) == 0 {
			indexErr = fmt.Errorf("the embedded brw skill is empty")
			return
		}
		sort.Strings(documents)
	})
	return documents, indexErr
}

func relativeTo(root, p string) (string, error) {
	cleaned := path.Clean(p)
	prefix := root + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("embedded skill path %q is outside %s", p, root)
	}
	return strings.TrimPrefix(cleaned, prefix), nil
}

// Documents lists every page of the skill, sorted, with SKILL.md first.
func Documents() ([]string, error) {
	all, err := index()
	if err != nil {
		return nil, err
	}
	ordered := make([]string, 0, len(all))
	ordered = append(ordered, Default)
	for _, name := range all {
		if name != Default {
			ordered = append(ordered, name)
		}
	}
	return ordered, nil
}

// Read returns one document of the skill, stamped with the version of the
// daemon serving it. An empty name is Default. An unknown name is an error that
// lists what there is, because an agent that guessed a path needs the real set
// more than it needs the refusal.
func Read(name, version string) (Document, error) {
	all, err := Documents()
	if err != nil {
		return Document{}, err
	}
	requested := strings.TrimSpace(name)
	if requested == "" {
		requested = Default
	}
	// The trailing separator forms are the same document; anything else is
	// compared verbatim against the embedded set, never normalised into it.
	requested = strings.TrimPrefix(requested, "./")
	if !contains(all, requested) {
		return Document{}, fmt.Errorf("the brw skill has no document %q; it has %s", name, strings.Join(all, ", "))
	}
	data, err := skills.FS.ReadFile(path.Join(embedRoot, requested))
	if err != nil {
		return Document{}, fmt.Errorf("read the embedded brw skill document %q: %w", requested, err)
	}
	return Document{
		Skill:     Name,
		Path:      requested,
		Version:   version,
		Source:    SourceEmbedded,
		Documents: all,
		Bytes:     len(data),
		Content:   string(data),
	}, nil
}

func contains(all []string, want string) bool {
	for _, name := range all {
		if name == want {
			return true
		}
	}
	return false
}

// FS exposes the embedded skill tree rooted at the skill directory, so an
// installer can write the binary's own copy to disk instead of hunting for a
// directory next to the executable.
func FS() (fs.FS, error) {
	return fs.Sub(skills.FS, embedRoot)
}
