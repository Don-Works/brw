package sessionstate

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MinKeyBytes is the shortest operator key the store accepts. The key is
	// stretched by HKDF, so this bounds the entropy an operator must supply
	// rather than the AES key length.
	MinKeyBytes = 32
	// DefaultTTL keeps a snapshot useful for a working day's worth of runs
	// without leaving a signed-in session sealed on disk indefinitely.
	DefaultTTL = 12 * time.Hour
	// MaxSnapshots bounds the store so List, which decrypts every file, stays
	// cheap and so a runaway caller cannot fill the disk with sealed sessions.
	MaxSnapshots = 64
	// maxSealedBytes bounds one snapshot. Cookies are small; a file larger than
	// this is a bug or an attack, and refusing it keeps the single-shot seal
	// honest (the whole plaintext is held in memory once).
	maxSealedBytes = 4 << 20

	sealMagic    = "brwstate1"
	sealSaltSize = 16
	sealKeyInfo  = "brw session state v1"
	fileSuffix   = ".state"
)

// ErrEncryptionKeyRequired is the fail-closed refusal. A snapshot of a
// signed-in session is exactly the thing that must not be written in the clear,
// so the store has no unencrypted mode to fall back to.
var ErrEncryptionKeyRequired = errors.New("session snapshots need an at-rest encryption key; start brwd with --state-key-file pointing at an owner-only file of at least 32 bytes")

var snapshotIDPattern = regexp.MustCompile(`^st_[0-9a-f]{32}$`)

type Config struct {
	Root string
	TTL  time.Duration
	// Key is the operator's at-rest key. An empty key is not a degraded mode:
	// NewStore refuses it.
	Key []byte
	// Now is a clock seam for tests. Nil means time.Now.
	Now func() time.Time
}

type Store struct {
	root string
	ttl  time.Duration
	key  []byte
	now  func() time.Time
	mu   sync.Mutex
}

// DefaultRoot is the browser host's own cache directory. It is deliberately not
// derived from the working directory: a snapshot must never land in a checkout.
func DefaultRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(base, "brw", "session-state"), nil
}

func NewStore(config Config) (*Store, error) {
	if len(config.Key) < MinKeyBytes {
		return nil, ErrEncryptionKeyRequired
	}
	root := strings.TrimSpace(config.Root)
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("session state root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create session state root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	// A root any local account can walk protects nothing that the encryption is
	// there to protect, so a pre-existing permissive directory is refused rather
	// than silently tightened under the operator.
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("session state root %s is reachable beyond its owner (mode %04o); chmod 700 it", root, info.Mode().Perm())
	}
	ttl := config.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Store{root: root, ttl: ttl, key: append([]byte(nil), config.Key...), now: now}, nil
}

func (s *Store) Root() string { return s.root }

// TTL is the store's retention ceiling; a per-save TTL may shorten it.
func (s *Store) TTL() time.Duration { return s.ttl }

// LoadKey reads the operator's key file. The rules mirror the artifact key: an
// absolute path, a regular file, unreadable by group and other, and outside the
// directory it protects — a key beside the ciphertext encrypts against nobody.
func LoadKey(path, stateRoot string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("session state key path is empty")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("session state key path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("session state key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("session state key %s is readable beyond its owner", filepath.Base(path))
	}
	if err := rejectKeyInsideRoot(path, stateRoot); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key := bytes.TrimRight(raw, " \t\r\n")
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("session state key must carry at least %d bytes of key material", MinKeyBytes)
	}
	return key, nil
}

func rejectKeyInsideRoot(path, stateRoot string) error {
	stateRoot = strings.TrimSpace(stateRoot)
	if stateRoot == "" {
		return nil
	}
	relative, err := filepath.Rel(filepath.Clean(stateRoot), filepath.Clean(filepath.Dir(path)))
	if err != nil {
		return nil
	}
	if relative == "." || !strings.HasPrefix(relative, "..") {
		return errors.New("session state key must not live inside the session state root")
	}
	return nil
}

type sealedDocument struct {
	Meta     Meta     `json:"meta"`
	Snapshot Snapshot `json:"snapshot"`
}

// SaveOptions carries the per-save narrowing. TTL may only shorten the store's
// retention, never lengthen it.
type SaveOptions struct {
	TTL time.Duration
}

// Save seals a snapshot and returns only its metadata.
func (s *Store) Save(snap Snapshot, opts SaveOptions) (Meta, error) {
	if len(snap.Origins) == 0 {
		return Meta{}, ErrNoOrigins
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sweepLocked(); err != nil {
		return Meta{}, err
	}
	existing, err := s.listLocked()
	if err != nil {
		return Meta{}, err
	}
	if len(existing) >= MaxSnapshots {
		return Meta{}, fmt.Errorf("session state store already holds %d snapshots (the cap); delete one before saving another", len(existing))
	}
	id, err := newSnapshotID()
	if err != nil {
		return Meta{}, err
	}
	now := s.now().UTC()
	ttl := s.ttl
	if opts.TTL > 0 && opts.TTL < ttl {
		ttl = opts.TTL
	}
	if snap.CapturedAt.IsZero() {
		snap.CapturedAt = now
	}
	meta := Meta{
		ID:          id,
		Origins:     append([]string(nil), snap.Origins...),
		CookieCount: len(snap.Cookies),
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	plain, err := json.Marshal(sealedDocument{Meta: meta, Snapshot: snap})
	if err != nil {
		return Meta{}, err
	}
	if len(plain) > maxSealedBytes {
		return Meta{}, fmt.Errorf("session snapshot is %d bytes, over the %d byte cap", len(plain), maxSealedBytes)
	}
	sealed, err := seal(s.key, id, plain)
	if err != nil {
		return Meta{}, err
	}
	if err := writeOwnerOnly(s.pathFor(id), sealed); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Load decrypts one snapshot. It is the restore path's door and nothing else:
// no MCP tool, HTTP route or CLI verb returns what it produces.
func (s *Store) Load(id string) (Snapshot, Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, meta, err := s.loadLocked(id)
	if err != nil {
		return Snapshot{}, Meta{}, err
	}
	return snap, meta, nil
}

func (s *Store) loadLocked(id string) (Snapshot, Meta, error) {
	if !snapshotIDPattern.MatchString(id) {
		return Snapshot{}, Meta{}, ErrNotFound
	}
	raw, err := os.ReadFile(s.pathFor(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, Meta{}, ErrNotFound
		}
		return Snapshot{}, Meta{}, err
	}
	plain, err := open(s.key, id, raw)
	if err != nil {
		return Snapshot{}, Meta{}, err
	}
	var doc sealedDocument
	if err := json.Unmarshal(plain, &doc); err != nil {
		return Snapshot{}, Meta{}, errors.New("session snapshot is unreadable")
	}
	if !s.now().UTC().Before(doc.Meta.ExpiresAt) {
		_ = os.Remove(s.pathFor(id))
		return Snapshot{}, Meta{}, ErrExpired
	}
	return doc.Snapshot, doc.Meta, nil
}

// List returns metadata for the live snapshots, expiring any that have lapsed.
func (s *Store) List() ([]Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sweepLocked(); err != nil {
		return nil, err
	}
	return s.listLocked()
}

func (s *Store) listLocked() ([]Meta, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), fileSuffix)
		if entry.IsDir() || id == entry.Name() {
			continue
		}
		_, meta, err := s.loadLocked(id)
		if err != nil {
			// A lapsed or unreadable file is not a reason to fail the listing;
			// sweepLocked has already removed what it could.
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Delete removes one snapshot. Deleting an unknown id is an error, so a caller
// that meant to revoke a session learns that it did not.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !snapshotIDPattern.MatchString(id) {
		return ErrNotFound
	}
	if err := os.Remove(s.pathFor(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Sweep removes every expired snapshot. Exported so a daemon can run it on a
// timer instead of waiting for the next call to notice.
func (s *Store) Sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked()
}

func (s *Store) sweepLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), fileSuffix)
		if entry.IsDir() || id == entry.Name() {
			continue
		}
		// loadLocked deletes an expired file on the way out; ignore the error,
		// which is exactly the expiry it reports.
		_, _, _ = s.loadLocked(id)
	}
	return nil
}

func (s *Store) pathFor(id string) string { return filepath.Join(s.root, id+fileSuffix) }

func newSnapshotID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "st_" + hex.EncodeToString(raw), nil
}

func writeOwnerOnly(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// seal is AES-256-GCM under a per-file key derived from the store key and a
// random salt. The snapshot id is the additional data, so a sealed file renamed
// onto another id fails authentication instead of restoring the wrong session.
func seal(storeKey []byte, id string, plain []byte) ([]byte, error) {
	salt := make([]byte, sealSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := sealAEAD(storeKey, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(sealMagic)+len(salt)+len(nonce)+len(plain)+aead.Overhead())
	out = append(out, sealMagic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, []byte(id)), nil
}

func open(storeKey []byte, id string, sealed []byte) ([]byte, error) {
	if len(sealed) < len(sealMagic)+sealSaltSize {
		return nil, errors.New("session snapshot is truncated")
	}
	if string(sealed[:len(sealMagic)]) != sealMagic {
		return nil, errors.New("session snapshot has an unrecognised format")
	}
	rest := sealed[len(sealMagic):]
	salt, rest := rest[:sealSaltSize], rest[sealSaltSize:]
	aead, err := sealAEAD(storeKey, salt)
	if err != nil {
		return nil, err
	}
	if len(rest) < aead.NonceSize() {
		return nil, errors.New("session snapshot is truncated")
	}
	nonce, body := rest[:aead.NonceSize()], rest[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, []byte(id))
	if err != nil {
		return nil, errors.New("session snapshot failed authentication")
	}
	return plain, nil
}

func sealAEAD(storeKey, salt []byte) (cipher.AEAD, error) {
	fileKey, err := hkdf.Key(sha256.New, storeKey, salt, sealKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
