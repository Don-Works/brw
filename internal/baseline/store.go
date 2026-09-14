package baseline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// Record is one stored baseline.
type Record struct {
	Key           Key               `json:"key"`
	Environment   Environment       `json:"environment"`
	Tree          snapshot.AriaTree `json:"aria_tree"`
	IgnoreRegions []IgnoreRegion    `json:"ignore_regions,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	// Screenshot lives beside the record as a PNG rather than base64 inside it,
	// so a baseline directory is inspectable with an image viewer.
	Screenshot []byte `json:"-"`
}

const (
	recordFile     = "baseline.json"
	screenshotFile = "screenshot.png"
	gitignoreFile  = ".gitignore"
)

// ErrRootInsideRepository refuses a baseline root that lives in a Git working
// tree. A baseline of a signed-in page is a screenshot of private content, and
// the failure mode is not subtle: someone runs a baseline update, `git add -A`
// picks up the PNGs, and a private page is in a public history forever.
var ErrRootInsideRepository = errors.New("baseline root is inside a Git working tree; baselines are private captures and must not be storable in a checkout — point --baseline-root at a directory outside any repository")

type Store struct {
	root string
	mu   sync.Mutex
}

// DefaultRoot puts baselines in the browser host's cache directory, outside any
// checkout by construction.
func DefaultRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(base, "brw", "baselines"), nil
}

func NewStore(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("baseline root must be absolute")
	}
	if err := rejectRootInsideRepository(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create baseline root: %w", err)
	}
	// Belt and braces for the case the refusal above cannot see: a repository
	// initialised around the root later. An ignore-everything file means a
	// `git add -A` in that repository still picks up nothing.
	if err := writeIfAbsent(filepath.Join(root, gitignoreFile), []byte("*\n")); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

// rejectRootInsideRepository walks up from the root looking for a .git entry.
// It checks the root itself and every ancestor, because a baseline written
// three directories below a repository root is just as committable.
func rejectRootInsideRepository(root string) error {
	current := filepath.Clean(root)
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return fmt.Errorf("%w (found %s)", ErrRootInsideRepository, filepath.Join(current, ".git"))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func writeIfAbsent(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func (s *Store) dirFor(key Key) string {
	return filepath.Join(s.root, key.digest(), fmt.Sprintf("step-%d", key.StepIndex), key.Environment.Fingerprint())
}

func (s *Store) scopeDir(digest string, step int) string {
	return filepath.Join(s.root, strings.ToLower(strings.TrimSpace(digest)), fmt.Sprintf("step-%d", step))
}

// Load returns the baseline stored for this exact key.
func (s *Store) Load(key Key) (Record, bool, error) {
	if err := key.Validate(); err != nil {
		return Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(s.dirFor(key))
}

func (s *Store) loadLocked(dir string) (Record, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return Record{}, false, fmt.Errorf("read baseline record: %w", err)
	}
	shot, err := os.ReadFile(filepath.Join(dir, screenshotFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, fmt.Errorf("baseline record in %s has no screenshot beside it", filepath.Base(dir))
		}
		return Record{}, false, err
	}
	record.Screenshot = shot
	return record, true, nil
}

// Save writes a baseline. It is called only from an explicit update: nothing in
// Check writes without the caller having asked for it.
func (s *Store) Save(record Record) error {
	if err := record.Key.Validate(); err != nil {
		return err
	}
	if len(record.Screenshot) == 0 {
		return errors.New("a baseline needs a screenshot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.dirFor(record.Key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	record.Environment = record.Key.Environment.Normalize()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, screenshotFile), record.Screenshot); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, recordFile), encoded)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// EnvironmentsFor lists the environments a baseline already exists under for
// this recipe step. It is what turns "no baseline for this key" into "you have
// a baseline, but it was captured at a different device pixel ratio".
func (s *Store) EnvironmentsFor(digest string, step int) ([]Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.scopeDir(digest, step))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Environment, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, ok, err := s.loadLocked(filepath.Join(s.scopeDir(digest, step), entry.Name()))
		if err != nil || !ok {
			continue
		}
		out = append(out, record.Environment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].canonical() < out[j].canonical() })
	return out, nil
}

// Delete removes one baseline, so a key that should no longer be gated stops
// failing runs.
func (s *Store) Delete(key Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.dirFor(key)
	if _, err := os.Stat(filepath.Join(dir, recordFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no baseline stored for that key")
		}
		return err
	}
	return os.RemoveAll(dir)
}
