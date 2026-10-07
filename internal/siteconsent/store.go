package siteconsent

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const storeVersion = 1

// RejectedRecord is one entry the store refused to load, kept so the reason can be shown rather than swallowed.
type RejectedRecord struct {
	Origin string `json:"origin"`
	Scope  Scope  `json:"scope,omitempty"`
	Reason string `json:"reason"`
}

type storeFile struct {
	Version int     `json:"version"`
	Grants  []Grant `json:"grants"`
}

// Store is the persistent set of consent records for one profile.
type Store struct {
	path      string
	key       []byte
	ledgerPth string

	mu        sync.Mutex
	grants    []Grant
	rejected  []RejectedRecord
	loadedSum [sha256.Size]byte
	loaded    bool
}

// OpenStore loads (or starts) the consent store at path, keyed by keyPath.
func OpenStore(path, keyPath string) (*Store, error) {
	key, err := LoadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	store := &Store{path: path, key: key, ledgerPth: LedgerPathFor(path)}
	if err := store.reload(); err != nil {
		return nil, err
	}
	return store, nil
}

// NewStoreWithKey builds a store over an explicit key, for callers that manage key material themselves (tests, and a managed machine that provisions the key out of band).
func NewStoreWithKey(path string, key []byte) (*Store, error) {
	if len(key) < keyBytes {
		return nil, fmt.Errorf("consent key holds %d bytes; at least %d are required", len(key), keyBytes)
	}
	store := &Store{path: path, key: append([]byte(nil), key...), ledgerPth: LedgerPathFor(path)}
	if err := store.reload(); err != nil {
		return nil, err
	}
	return store, nil
}

// Path is the grant file this store reads and writes.
func (s *Store) Path() string { return s.path }

// LedgerPathFor names the append-only ledger beside a grant file.
func LedgerPathFor(grantPath string) string {
	dir := filepath.Dir(grantPath)
	base := strings.TrimSuffix(filepath.Base(grantPath), filepath.Ext(grantPath))
	return filepath.Join(dir, base+"-ledger.jsonl")
}

func (s *Store) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked()
}

func (s *Store) parseLocked(data []byte) error {
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("read consent store %s: %w", s.path, err)
	}
	if file.Version > storeVersion {
		return fmt.Errorf("consent store %s is version %d; this brw understands version %d", s.path, file.Version, storeVersion)
	}
	grants := make([]Grant, 0, len(file.Grants))
	rejected := make([]RejectedRecord, 0)
	for _, grant := range file.Grants {
		if err := grant.Valid(); err != nil {
			rejected = append(rejected, RejectedRecord{Origin: grant.Origin, Scope: grant.Scope, Reason: err.Error()})
			continue
		}
		if err := grant.Verify(s.key); err != nil {
			rejected = append(rejected, RejectedRecord{Origin: grant.Origin, Scope: grant.Scope, Reason: err.Error()})
			continue
		}
		grants = append(grants, grant)
	}
	s.grants = grants
	s.rejected = rejected
	s.loadedSum = sha256.Sum256(data)
	s.loaded = true
	return nil
}

func (s *Store) refreshLocked() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.grants, s.rejected, s.loaded = nil, nil, false
		return nil
	}
	if err == nil {
		if s.loaded && sha256.Sum256(data) == s.loadedSum {
			return nil
		}
		err = s.parseLocked(data)
	}
	if err != nil {
		s.grants, s.loaded = nil, false
		s.rejected = []RejectedRecord{{Reason: err.Error()}}
	}
	return err
}

// List returns every loaded record, newest first.
func (s *Store) List() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.refreshLocked()
	out := append([]Grant(nil), s.grants...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out
}

// Rejected returns the records refused on the last load.
func (s *Store) Rejected() []RejectedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.refreshLocked()
	return append([]RejectedRecord(nil), s.rejected...)
}

// Lookup returns the record that decides origin at scope want, and whether one was found.
func (s *Store) Lookup(origin string, want Scope, now time.Time) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.refreshLocked()
	var best Grant
	var found bool
	for _, grant := range s.grants {
		if grant.Origin != origin || grant.Expired(now) {
			continue
		}
		if grant.Decision == DecisionDeny {
			if !want.covers(grant.Scope) {
				continue
			}
		} else if !grant.Scope.covers(want) {
			continue
		}

		if grant.Verify(s.key) != nil {
			continue
		}
		if grant.Decision == DecisionDeny {
			return grant, true
		}
		if !found || grant.GrantedAt.After(best.GrantedAt) {
			best, found = grant, true
		}
	}
	return best, found
}

// Record writes one decision, replacing any record for the same origin+scope.
func (s *Store) Record(grant Grant) (Grant, error) {
	origin, err := CanonicalOrigin(grant.Origin)
	if err != nil {
		return Grant{}, err
	}
	if origin == "" {
		return Grant{}, fmt.Errorf("%q has no network origin to consent to", grant.Origin)
	}
	grant.Origin = origin
	if grant.GrantedAt.IsZero() {
		grant.GrantedAt = time.Now().UTC()
	}
	grant.GrantedAt = grant.GrantedAt.UTC()
	if !grant.Expiry.IsZero() {
		grant.Expiry = grant.Expiry.UTC()
	}
	if grant.Decision == "" {
		grant.Decision = DecisionAllow
	}
	if err := grant.Valid(); err != nil {
		return Grant{}, err
	}
	grant.Sign(s.key)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return Grant{}, err
	}
	kept := make([]Grant, 0, len(s.grants)+1)
	for _, existing := range s.grants {
		if existing.Origin == grant.Origin && existing.Scope == grant.Scope {
			continue
		}
		kept = append(kept, existing)
	}
	kept = append(kept, grant)
	if err := s.writeLocked(kept); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

// Revoke removes the records for one origin.
func (s *Store) Revoke(origin string, scope Scope) (int, error) {
	canonical, err := CanonicalOrigin(origin)
	if err != nil {
		return 0, err
	}
	if canonical == "" {
		return 0, fmt.Errorf("%q has no network origin to revoke", origin)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return 0, err
	}
	kept := make([]Grant, 0, len(s.grants))
	removed := 0
	for _, existing := range s.grants {
		if existing.Origin == canonical && (scope == "" || existing.Scope == scope) {
			removed++
			continue
		}
		kept = append(kept, existing)
	}
	if removed == 0 {
		return 0, nil
	}
	if err := s.writeLocked(kept); err != nil {
		return 0, err
	}
	return removed, nil
}

// RevokeAll drops every record and reports how many went.
func (s *Store) RevokeAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return 0, err
	}
	removed := len(s.grants)
	if removed == 0 {
		return 0, nil
	}
	if err := s.writeLocked(nil); err != nil {
		return 0, err
	}
	return removed, nil
}

func (s *Store) writeLocked(grants []Grant) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	sorted := append([]Grant(nil), grants...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Origin != sorted[j].Origin {
			return sorted[i].Origin < sorted[j].Origin
		}
		return sorted[i].Scope < sorted[j].Scope
	})
	payload, err := json.MarshalIndent(storeFile{Version: storeVersion, Grants: sorted}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".site-grants-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return err
	}
	s.grants = sorted
	s.loadedSum = sha256.Sum256(payload)
	s.loaded = true
	return nil
}

// LedgerEntry is one line of the append-only consent ledger.
type LedgerEntry struct {
	At               time.Time `json:"at"`
	Event            string    `json:"event"`
	Origin           string    `json:"origin"`
	Scope            Scope     `json:"scope,omitempty"`
	Decision         Decision  `json:"decision,omitempty"`
	Actor            string    `json:"actor"`
	OverrideCategory string    `json:"override_category,omitempty"`
	Reason           string    `json:"reason,omitempty"`
}

// AppendLedger records one consent event.
func (s *Store) AppendLedger(entry LedgerEntry) error {
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	entry.At = entry.At.UTC()
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.ledgerPth), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(s.ledgerPth, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}

// Ledger reads the recorded consent events, oldest first.
func (s *Store) Ledger() ([]LedgerEntry, error) {
	data, err := os.ReadFile(s.ledgerPth)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []LedgerEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry LedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("read consent ledger %s: %w", s.ledgerPth, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
