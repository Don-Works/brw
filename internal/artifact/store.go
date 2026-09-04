// Package artifact stores large or sensitive browser observations outside MCP
// responses. Tool calls return small, opaque metadata handles; bytes enter a
// model context only through an explicit bounded read.
package artifact

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxArtifactBytes = int64(128 << 20)
	DefaultMaxTotalBytes    = int64(2 << 30)
	DefaultTTL              = 24 * time.Hour
	MaxReadBytes            = 1 << 20
	orphanGrace             = 5 * time.Minute
)

// Meta is deliberately payload-free and safe to return from MCP and HTTP.
type Meta struct {
	ID         string    `json:"artifact_id"`
	Kind       string    `json:"kind"`
	MIMEType   string    `json:"mime_type"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	SourceHash string    `json:"source_hash,omitempty"`
	Redaction  string    `json:"redaction,omitempty"`
}

type PutOptions struct {
	Kind       string
	MIMEType   string
	SourceHash string
	Redaction  string
	// TTL may shorten the configured retention for a particularly sensitive
	// artifact. Zero uses the store default; it may never lengthen retention.
	TTL time.Duration
}

type TextHit struct {
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

type Chunk struct {
	ArtifactID string `json:"artifact_id"`
	Offset     int64  `json:"offset"`
	SizeBytes  int    `json:"size_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	Text       string `json:"text,omitempty"`
	Base64     string `json:"base64,omitempty"`
	Encoding   string `json:"encoding"`
	More       bool   `json:"more"`
	NextOffset int64  `json:"next_offset,omitempty"`
}

type Config struct {
	Root             string
	MaxArtifactBytes int64
	MaxTotalBytes    int64
	TTL              time.Duration
}

type Store struct {
	root             string
	maxArtifactBytes int64
	maxTotalBytes    int64
	ttl              time.Duration
	now              func() time.Time
	mu               sync.Mutex
}

var artifactIDPattern = regexp.MustCompile(`^art_[0-9a-f]{32}$`)

func DefaultRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(base, "brw", "artifacts"), nil
}

func NewStore(config Config) (*Store, error) {
	if strings.TrimSpace(config.Root) == "" {
		root, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		config.Root = root
	}
	if !filepath.IsAbs(config.Root) {
		return nil, errors.New("artifact root must be absolute")
	}
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if config.MaxTotalBytes == 0 {
		config.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if config.TTL == 0 {
		config.TTL = DefaultTTL
	}
	if config.MaxArtifactBytes < 1 || config.MaxTotalBytes < config.MaxArtifactBytes || config.TTL < time.Second {
		return nil, errors.New("invalid artifact size or retention limits")
	}
	if info, err := os.Lstat(config.Root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("artifact root must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := rejectGitCheckout(config.Root); err != nil {
		return nil, err
	}
	prospective, err := resolveArtifactRoot(config.Root)
	if err != nil {
		return nil, err
	}
	if err := rejectBroadArtifactRoot(prospective); err != nil {
		return nil, err
	}
	if err := rejectGitCheckout(prospective); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.Root, 0o700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(config.Root)
	if err != nil {
		return nil, err
	}
	// A non-symlink leaf can still sit below a symlinked parent. Re-check the
	// canonical path after creation so /outside/link-to-repo/subdir/artifacts
	// cannot bypass the pre-create checkout walk.
	if err := rejectGitCheckout(resolved); err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("artifact root must be a directory")
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		return nil, err
	}
	store := &Store{
		root: resolved, maxArtifactBytes: config.MaxArtifactBytes,
		maxTotalBytes: config.MaxTotalBytes, ttl: config.TTL, now: time.Now,
	}
	if err := store.reconcileOrphansLocked(); err != nil {
		return nil, fmt.Errorf("reconcile artifact cache: %w", err)
	}
	if _, err := store.purgeExpiredLocked(); err != nil {
		return nil, fmt.Errorf("purge expired artifact cache: %w", err)
	}
	return store, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Put(opts PutOptions, src io.Reader) (Meta, error) {
	return s.PutContext(context.Background(), opts, src)
}

func (s *Store) PutContext(ctx context.Context, opts PutOptions, src io.Reader) (Meta, error) {
	if src == nil {
		return Meta{}, errors.New("artifact source is nil")
	}
	if ctx == nil {
		return Meta{}, errors.New("artifact context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	if len(opts.Redaction) > 256 || !utf8.ValidString(opts.Redaction) {
		return Meta{}, errors.New("artifact redaction label must be valid UTF-8 and at most 256 bytes")
	}
	mediaType, err := validateKind(opts.Kind, opts.MIMEType)
	if err != nil {
		return Meta{}, err
	}
	retention := s.ttl
	if opts.TTL != 0 {
		if opts.TTL < time.Second || opts.TTL > s.ttl {
			return Meta{}, errors.New("artifact TTL must be between one second and the store retention")
		}
		retention = opts.TTL
	}
	id, err := newID()
	if err != nil {
		return Meta{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reconcileOrphansLocked(); err != nil {
		return Meta{}, err
	}
	if _, err := s.purgeExpiredLocked(); err != nil {
		return Meta{}, err
	}
	used, err := s.bytesUsedLocked()
	if err != nil {
		return Meta{}, err
	}
	remaining := s.maxTotalBytes - used
	if remaining <= 0 {
		return Meta{}, errors.New("artifact store quota exhausted")
	}
	limit := min(s.maxArtifactBytes, remaining)

	tmp, err := os.CreateTemp(s.root, ".artifact-*")
	if err != nil {
		return Meta{}, err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return Meta{}, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(contextReader{ctx: ctx, reader: src}, limit+1))
	if err != nil {
		return Meta{}, err
	}
	if written > limit {
		if limit < s.maxArtifactBytes {
			return Meta{}, errors.New("artifact store quota exhausted")
		}
		return Meta{}, fmt.Errorf("artifact exceeds %d-byte limit", s.maxArtifactBytes)
	}
	if err := tmp.Sync(); err != nil {
		return Meta{}, err
	}
	if err := tmp.Close(); err != nil {
		return Meta{}, err
	}
	if err := os.Rename(tmpName, s.blobPath(id)); err != nil {
		return Meta{}, err
	}
	committed = true
	created := s.now().UTC()
	meta := Meta{
		ID: id, Kind: opts.Kind, MIMEType: mediaType, SizeBytes: written,
		SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: created,
		ExpiresAt: created.Add(retention), SourceHash: opts.SourceHash,
		Redaction: opts.Redaction,
	}
	if err := s.writeMetaLocked(meta); err != nil {
		_ = os.Remove(s.blobPath(id))
		return Meta{}, err
	}
	return meta, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Store) Info(id string) (Meta, error) {
	if err := validateID(id); err != nil {
		return Meta{}, err
	}
	b, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return Meta{}, err
	}
	if !validStoredMeta(meta, id) {
		return Meta{}, errors.New("artifact metadata is invalid")
	}
	if !s.now().Before(meta.ExpiresAt) {
		_ = s.Delete(id)
		return Meta{}, os.ErrNotExist
	}
	return meta, nil
}

func (s *Store) Read(id string, offset int64, maxBytes int) ([]byte, Meta, bool, error) {
	if offset < 0 || maxBytes < 1 || maxBytes > MaxReadBytes {
		return nil, Meta{}, false, errors.New("invalid artifact read window")
	}
	meta, err := s.Info(id)
	if err != nil {
		return nil, Meta{}, false, err
	}
	f, err := s.openBlob(id)
	if err != nil {
		return nil, Meta{}, false, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, Meta{}, false, err
	}
	buf := make([]byte, maxBytes+1)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, Meta{}, false, err
	}
	more := n > maxBytes
	if more {
		n = maxBytes
	}
	return buf[:n], meta, more, nil
}

func (s *Store) SearchText(id, query string, limit int) ([]TextHit, error) {
	return s.SearchTextContext(context.Background(), id, query, limit)
}

func (s *Store) SearchTextContext(ctx context.Context, id, query string, limit int) ([]TextHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 || !utf8.ValidString(query) || limit < 1 || limit > 100 {
		return nil, errors.New("invalid artifact search")
	}
	matcher, err := regexp.Compile("(?i:" + regexp.QuoteMeta(query) + ")")
	if err != nil {
		return nil, errors.New("invalid artifact search")
	}
	meta, err := s.Info(id)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(meta.MIMEType, "text/") && meta.MIMEType != "application/json" {
		return nil, errors.New("artifact is not searchable text")
	}
	f, err := s.openBlob(id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	hits := make([]TextHit, 0, limit)
	line := 1
	// Search one bounded fragment at a time. Page text and downloaded JSON can
	// legitimately contain a line tens of megabytes long; bufio.Scanner's token
	// ceiling would make the whole artifact unsearchable. Keeping only the tail
	// needed for a cross-fragment match bounds memory independently of line size.
	tail := ""
	lineMatched := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fragmentBytes, readErr := reader.ReadSlice('\n')
		fragment := string(fragmentBytes)
		if fragment != "" && !lineMatched {
			value := tail + fragment
			position := matcher.FindStringIndex(value)
			if position != nil {
				start := max(0, position[0]-80)
				end := min(len(value), position[1]+160)
				excerpt := strings.ToValidUTF8(strings.TrimSuffix(value[start:end], "\n"), "�")
				hits = append(hits, TextHit{Line: line, Excerpt: excerpt})
				lineMatched = true
				if len(hits) == limit {
					return hits, nil
				}
			}
			// A Unicode case partner can use more UTF-8 bytes than the query rune.
			// Four bytes per query byte is a small, safe overlap bound.
			keep := min(len(value), max(0, len(query)*4))
			tail = value[len(value)-keep:]
		}
		if strings.HasSuffix(fragment, "\n") {
			line++
			tail = ""
			lineMatched = false
		}
		if errors.Is(readErr, io.EOF) {
			return hits, nil
		}
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) {
			return nil, readErr
		}
	}
}

func (s *Store) Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(id)
}

func (s *Store) PurgeExpired() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeExpiredLocked()
}

// RunJanitor physically purges expired artifacts even when the daemon is idle.
// Expiry is still enforced synchronously by Info/Read/Search; interval only
// bounds how long inaccessible bytes may remain on disk. The loop reports an
// isolated scan error and continues so one damaged metadata file cannot disable
// retention for every other artifact until restart.
func (s *Store) RunJanitor(ctx context.Context, interval time.Duration, report func(error)) {
	if interval <= 0 {
		if report != nil {
			report(errors.New("artifact janitor interval must be positive"))
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.PurgeExpired(); err != nil && report != nil {
				report(err)
			}
		}
	}
}

func (s *Store) purgeExpiredLocked() (int, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	now := s.now()
	purged := 0
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !artifactIDPattern.MatchString(id) {
			continue
		}
		b, readErr := os.ReadFile(s.metaPath(id))
		var meta Meta
		if readErr != nil || json.Unmarshal(b, &meta) != nil || !validStoredMeta(meta, id) {
			// A final .json file is installed only by atomic rename after a complete
			// write+fsync. It cannot be an in-process partial commit; retaining a
			// malformed pair would make every reopen and Put fail forever. Treat the
			// pair as corrupt cache debris and remove it without disturbing healthy
			// artifacts.
			if deleteErr := s.deleteLocked(id); deleteErr != nil {
				errs = append(errs, fmt.Errorf("remove corrupt artifact %s: %w", id, deleteErr))
			}
			continue
		}
		if !now.Before(meta.ExpiresAt) {
			if deleteErr := s.deleteLocked(id); deleteErr != nil {
				errs = append(errs, deleteErr)
			} else {
				purged++
			}
		}
	}
	return purged, errors.Join(errs...)
}

func validStoredMeta(meta Meta, id string) bool {
	return meta.ID == id && meta.SizeBytes >= 0 && !meta.ExpiresAt.IsZero()
}

func (s *Store) bytesUsedLocked() (int64, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".blob") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

// reconcileOrphansLocked repairs the only unavoidable gap in the two-file
// commit: a crash may land a blob just before its metadata rename, or remove a
// blob just before its metadata during deletion. Young singletons are left
// alone because another daemon could be inside that tiny commit window; stale
// singletons and abandoned temp files are cache debris and are removed.
func (s *Store) reconcileOrphansLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	present := make(map[string]map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		for _, suffix := range []string{".blob", ".json"} {
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			id := strings.TrimSuffix(name, suffix)
			if artifactIDPattern.MatchString(id) {
				if present[id] == nil {
					present[id] = map[string]bool{}
				}
				present[id][suffix] = true
			}
		}
	}
	cutoff := s.now().Add(-orphanGrace)
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(s.root, name)
		stale := false
		if strings.HasPrefix(name, ".artifact-") || strings.HasPrefix(name, ".meta-") || strings.HasPrefix(name, ".video-") {
			stale = true
		} else {
			for _, suffix := range []string{".blob", ".json"} {
				if strings.HasSuffix(name, suffix) {
					id := strings.TrimSuffix(name, suffix)
					stale = artifactIDPattern.MatchString(id) && len(present[id]) == 1
					break
				}
			}
		}
		if !stale {
			continue
		}
		info, infoErr := os.Lstat(path)
		if infoErr != nil {
			if !errors.Is(infoErr, os.ErrNotExist) {
				errs = append(errs, infoErr)
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			errs = append(errs, removeErr)
		}
	}
	return errors.Join(errs...)
}

func (s *Store) writeMetaLocked(meta Meta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.metaPath(meta.ID)); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Store) openBlob(id string) (*os.File, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	path := s.blobPath(id)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("artifact blob is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("artifact blob changed before it was opened")
	}
	return file, nil
}

func (s *Store) deleteLocked(id string) error {
	var errs []error
	for _, path := range []string{s.blobPath(id), s.metaPath(id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Store) blobPath(id string) string { return filepath.Join(s.root, id+".blob") }
func (s *Store) metaPath(id string) string { return filepath.Join(s.root, id+".json") }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "art_" + hex.EncodeToString(b), nil
}

func validateID(id string) error {
	if !artifactIDPattern.MatchString(id) {
		return errors.New("invalid artifact id")
	}
	return nil
}

func validateKind(kind, rawMIME string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(rawMIME))
	if err != nil || mediaType == "" {
		return "", errors.New("invalid artifact MIME type")
	}
	allowed := map[string][]string{
		"text":          {"text/plain", "text/markdown", "text/html"},
		"semantic_json": {"application/json"},
		"screenshot":    {"image/png", "image/jpeg", "image/webp"},
		"pdf":           {"application/pdf"},
		"video":         {"video/webm", "video/mp4"},
		"download":      nil,
	}
	mimes, ok := allowed[kind]
	if !ok {
		return "", fmt.Errorf("unsupported artifact kind %q", kind)
	}
	if mimes == nil {
		return mediaType, nil
	}
	for _, candidate := range mimes {
		if mediaType == candidate {
			return mediaType, nil
		}
	}
	sort.Strings(mimes)
	return "", fmt.Errorf("artifact kind %q does not accept MIME type %q", kind, mediaType)
}

// rejectGitCheckout prevents the easiest accidental leak: pointing the artifact
// store anywhere inside a checkout and later committing it. A private registry
// can still be versioned separately; volatile page artifacts should not be.
func rejectGitCheckout(path string) error {
	current := filepath.Clean(path)
	for {
		if info, err := os.Stat(filepath.Join(current, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return errors.New("artifact root must be outside a git checkout")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// rejectBroadArtifactRoot prevents a typo such as --artifact-dir=/ or a home,
// cache, config, or temporary-directory root from having its permissions
// changed or being filled with opaque blobs. The store must own a dedicated
// child directory.
func rejectBroadArtifactRoot(path string) error {
	clean := filepath.Clean(path)
	volumeRoot := filepath.Clean(filepath.VolumeName(clean) + string(filepath.Separator))
	if clean == volumeRoot || filepath.Dir(clean) == volumeRoot {
		return errors.New("artifact root is too broad; use a dedicated child directory")
	}
	for _, resolve := range []func() (string, error){os.UserHomeDir, os.UserCacheDir, os.UserConfigDir} {
		candidate, err := resolve()
		if err == nil && (clean == filepath.Clean(candidate) || filepath.Dir(clean) == filepath.Clean(candidate)) {
			return errors.New("artifact root is too broad; use a dedicated child directory")
		}
	}
	if temp := filepath.Clean(os.TempDir()); clean == temp || filepath.Dir(clean) == temp {
		return errors.New("artifact root is too broad; use a dedicated child directory")
	}
	return nil
}

// resolveArtifactRoot canonicalizes the existing prefix and then appends any
// not-yet-created path components. filepath.EvalSymlinks alone cannot inspect a
// prospective directory, which is exactly where a symlinked parent could hide
// a checkout boundary.
func resolveArtifactRoot(path string) (string, error) {
	current := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
