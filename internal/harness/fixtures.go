// Package harness is the rig the measurement harnesses run on: a deterministic
// local fixture origin, a disposable headless browser, a metered CDP transport,
// and an environment fingerprint.
//
// It is separate from the harnesses themselves (internal/bench,
// internal/agenteval) because a latency measurement and an agent evaluation
// need the same rig and have to agree on what "the same environment" means —
// two records are only comparable when they name the same browser build, the
// same machine and the same fixture bytes.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fixtures serves tests/fixtures over loopback HTTP.
//
// The harnesses use an http origin rather than file:// URLs because a file page
// is a different security context: cookies are not stored, fetch is blocked and
// same-origin rules differ, so a number measured there would not describe the
// browsing brw actually does. Nothing beyond this listener is contacted during
// a run.
type Fixtures struct {
	ln   net.Listener
	srv  *http.Server
	root string
	base string
}

// ServeFixtures starts the fixture origin for the tests/fixtures directory
// under repoRoot.
func ServeFixtures(repoRoot string) (*Fixtures, error) {
	root := filepath.Join(repoRoot, "tests", "fixtures")
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("fixture root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fixture root %s is not a directory", root)
	}
	// Resolve the root once so the per-request containment check below compares
	// real paths to a real path.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Fixtures{ln: ln, root: resolvedRoot, base: "http://" + ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.serve)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// BaseURL is the origin fixtures are served from.
func (f *Fixtures) BaseURL() string { return f.base }

// URL returns the address of one fixture file, for example "forms.html".
func (f *Fixtures) URL(name string) string {
	return f.base + "/" + strings.TrimPrefix(name, "/")
}

// Close stops the listener.
func (f *Fixtures) Close() error {
	if f == nil || f.srv == nil {
		return nil
	}
	return f.srv.Close()
}

// Digest fingerprints the served bytes, so a record states which fixture
// content produced it. Two runs of the same harness against different fixture
// content are not comparable, and without this nothing would say so.
func (f *Fixtures) Digest() (string, error) {
	return DigestDir(f.root)
}

// DigestDir hashes every regular file under dir, name and content, in a stable
// order.
func DigestDir(dir string) (string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%s\n%d\n", filepath.ToSlash(rel), len(data))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (f *Fixtures) serve(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if rel == "" || rel == "." {
		http.NotFound(w, r)
		return
	}
	target, err := f.resolve(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, target)
}

// resolve maps a request path to a file inside the fixture root.
//
// The containment check is on the RESOLVED path, not on the requested string:
// a "../" prefix, an absolute path and a symlink pointing out of the root all
// have to survive the same comparison, and a string check only catches the
// first two. A hard link created inside the root is inside the root by
// definition and is not something this can distinguish.
func (f *Fixtures) resolve(rel string) (string, error) {
	joined := filepath.Join(f.root, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(f.root, resolved)
	if err != nil {
		return "", err
	}
	if within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return "", errors.New("path escapes the fixture root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	return resolved, nil
}

// Reachable confirms the fixture origin answers, so a harness fails on its own
// listener rather than inside a browser action several seconds later.
func (f *Fixtures) Reachable(name string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(f.URL(name))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fixture %s returned %s", name, resp.Status)
	}
	return nil
}
