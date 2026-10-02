package approval

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	Pending      = "pending"
	Approved     = "approved"
	Denied       = "denied"
	Consumed     = "consumed"
	Expired      = "expired"
	Stale        = "stale"
	maxRequests  = 1000
	defaultTTL   = 10 * time.Minute
	maxTTL       = time.Hour
	maxArguments = 64 << 10
	maxFileSize  = 80 << 20
)

// Request records a sanitized approval request and its durable disposition.
type Request struct {
	ID           string          `json:"id"`
	Fingerprint  string          `json:"fingerprint"`
	Tool         string          `json:"tool"`
	Origin       string          `json:"origin"`
	TabID        string          `json:"tab_id"`
	SessionID    string          `json:"session_id"`
	Summary      string          `json:"summary"`
	StateDigest  string          `json:"state_digest"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	DecidedAt    time.Time       `json:"decided_at"`
	Status       string          `json:"status"`
	DecisionBy   string          `json:"decision_by"`
	DecisionNote string          `json:"decision_note"`
}

// Error identifies a refused operation without requiring callers to parse prose.
type Error struct {
	Code      string
	RequestID string
	Message   string
}

// Error returns the refusal message.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Store serializes approval transitions and persists them before returning success.
type Store struct {
	mu       sync.Mutex
	path     string
	lock     *os.File
	requests map[string]Request
	closed   bool
	fault    error
	now      func() time.Time
	save     func(map[string]Request) error
}

type snapshot struct {
	Version  int       `json:"version"`
	Requests []Request `json:"requests"`
}

func refusal(code, id, message string) error {
	return &Error{Code: code, RequestID: id, Message: message}
}

// Open acquires an exclusive lock on a JSON store in a private directory.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, refusal("invalid_request", "", "store path is empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, refusal("invalid_request", "", "store directory must be private")
	}
	lock, err := openPrivate(path+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err = acquireLock(lock); err != nil {
		lock.Close()
		return nil, refusal("store_locked", "", err.Error())
	}
	s := &Store{path: path, lock: lock, requests: make(map[string]Request), now: time.Now}
	s.save = s.persist
	if err := s.load(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func openPrivate(path string, flags int) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, refusal("invalid_request", "", "store files must be private regular files")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, flags, 0600)
}

func (s *Store) load() error {
	f, err := openPrivate(s.path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxFileSize {
		return refusal("store_corrupt", "", "store exceeds size limit")
	}
	var disk snapshot
	if err := json.Unmarshal(data, &disk); err != nil {
		return refusal("store_corrupt", "", err.Error())
	}
	if disk.Version != 1 || len(disk.Requests) > maxRequests || disk.Requests == nil {
		return refusal("store_corrupt", "", "invalid store version or request count")
	}
	for _, r := range disk.Requests {
		if err := validate(r); err != nil {
			return refusal("store_corrupt", r.ID, err.Error())
		}
		if _, exists := s.requests[r.ID]; exists {
			return refusal("store_corrupt", r.ID, "duplicate request identifier")
		}
		s.requests[r.ID] = r
	}
	return nil
}

func validate(r Request) error {
	if r.ID == "" || r.Fingerprint == "" || r.Tool == "" || r.StateDigest == "" {
		return errors.New("missing request binding")
	}
	if r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > maxTTL {
		return errors.New("invalid request lifetime")
	}
	if len(r.Arguments) > maxArguments || len(r.Arguments) > 0 && !json.Valid(r.Arguments) {
		return errors.New("invalid display arguments")
	}
	switch r.Status {
	case Pending, Approved, Denied, Consumed, Expired, Stale:
	default:
		return errors.New("invalid request status")
	}
	return nil
}

func copyRequest(r Request) Request {
	r.Arguments = append(json.RawMessage(nil), r.Arguments...)
	return r
}

func (s *Store) ready() error {
	if s.closed {
		return refusal("store_closed", "", "store is closed")
	}
	return s.fault
}

func (s *Store) working() (map[string]Request, bool) {
	requests := make(map[string]Request, len(s.requests))
	changed := false
	now := s.now()
	for id, r := range s.requests {
		if (r.Status == Pending || r.Status == Approved) && !now.Before(r.ExpiresAt) {
			r.Status = Expired
			changed = true
		}
		requests[id] = r
	}
	return requests, changed
}

func (s *Store) commit(requests map[string]Request, changed bool) error {
	if !changed {
		return nil
	}
	if err := s.save(requests); err != nil {
		s.fault = refusal("persistence_failed", "", err.Error())
		return s.fault
	}
	s.requests = requests
	return nil
}

func (s *Store) result(requests map[string]Request, changed bool, r Request, err error) (Request, error) {
	if saveErr := s.commit(requests, changed); saveErr != nil {
		return Request{}, saveErr
	}
	return copyRequest(r), err
}

// Enqueue creates a request or returns its unexpired matching disposition.
func (s *Store) Enqueue(r Request) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Request{}, err
	}
	requests, changed := s.working()
	for _, existing := range requests {
		if existing.Fingerprint == r.Fingerprint && s.now().Before(existing.ExpiresAt) && existing.Status != Stale && existing.Status != Expired {
			return s.result(requests, changed, existing, nil)
		}
	}
	r.CreatedAt = s.now().UTC()
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = r.CreatedAt.Add(defaultTTL)
	}
	if r.ExpiresAt.After(r.CreatedAt.Add(maxTTL)) {
		r.ExpiresAt = r.CreatedAt.Add(maxTTL)
	}
	r.Status, r.DecisionBy, r.DecisionNote = Pending, "", ""
	r.DecidedAt = time.Time{}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return s.result(requests, changed, Request{}, err)
	}
	r.ID = hex.EncodeToString(random[:])
	if err := validate(r); err != nil {
		return s.result(requests, changed, Request{}, refusal("invalid_request", "", err.Error()))
	}
	if len(requests) >= maxRequests {
		terminal := make([]Request, 0)
		for _, existing := range requests {
			if existing.Status == Stale || existing.Status == Expired || !s.now().Before(existing.ExpiresAt) {
				terminal = append(terminal, existing)
			}
		}
		sort.Slice(terminal, func(i, j int) bool {
			if terminal[i].CreatedAt.Equal(terminal[j].CreatedAt) {
				return terminal[i].ID < terminal[j].ID
			}
			return terminal[i].CreatedAt.Before(terminal[j].CreatedAt)
		})
		if len(terminal) == 0 {
			return s.result(requests, changed, Request{}, refusal("queue_full", "", "approval queue is full"))
		}
		delete(requests, terminal[0].ID)
	}
	r = copyRequest(r)
	requests[r.ID] = r
	return s.result(requests, true, r, nil)
}

// List returns detached requests, newest first, persisting any observed expirations.
func (s *Store) List() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready() != nil {
		return nil
	}
	requests, changed := s.working()
	if s.commit(requests, changed) != nil {
		return nil
	}
	return ordered(requests)
}

func ordered(requests map[string]Request) []Request {
	result := make([]Request, 0, len(requests))
	for _, r := range requests {
		result = append(result, copyRequest(r))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Get returns a detached request, persisting any observed expirations.
func (s *Store) Get(id string) (Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready() != nil {
		return Request{}, false
	}
	requests, changed := s.working()
	if s.commit(requests, changed) != nil {
		return Request{}, false
	}
	r, found := requests[id]
	return copyRequest(r), found
}

// Decide durably approves or denies a pending request without reversing a decision.
func (s *Store) Decide(id, decision, actor, note string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Request{}, err
	}
	requests, changed := s.working()
	r, found := requests[id]
	if !found {
		return s.result(requests, changed, Request{}, refusal("not_found", id, "approval request not found"))
	}
	if decision != Approved && decision != Denied {
		return s.result(requests, changed, r, refusal("invalid_request", id, "decision must be approved or denied"))
	}
	if r.Status == decision {
		return s.result(requests, changed, r, nil)
	}
	if r.Status != Pending {
		return s.result(requests, changed, r, statusError(r))
	}
	r.Status, r.DecisionBy, r.DecisionNote, r.DecidedAt = decision, actor, note, s.now().UTC()
	requests[id] = r
	return s.result(requests, true, r, nil)
}

func statusError(r Request) error { return refusal(r.Status, r.ID, "approval request is "+r.Status) }

// Consume durably spends one approved request after verifying its action and state.
func (s *Store) Consume(id, fingerprint, stateDigest string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Request{}, err
	}
	requests, changed := s.working()
	r, found := requests[id]
	if !found {
		return s.result(requests, changed, Request{}, refusal("not_found", id, "approval request not found"))
	}
	if r.Status != Approved {
		return s.result(requests, changed, r, statusError(r))
	}
	if r.Fingerprint != fingerprint || r.StateDigest != stateDigest {
		r.Status, r.DecisionNote = Stale, "approval bindings changed"
		requests[id] = r
		return s.result(requests, true, r, statusError(r))
	}
	r.Status = Consumed
	requests[id] = r
	return s.result(requests, true, r, nil)
}

// Invalidate marks a pending or approved request stale without resurrecting terminals.
func (s *Store) Invalidate(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	requests, changed := s.working()
	r, found := requests[id]
	var err error
	if !found {
		err = refusal("not_found", id, "approval request not found")
	} else if r.Status == Pending || r.Status == Approved {
		r.Status, r.DecisionNote = Stale, reason
		requests[id], changed = r, true
	} else if r.Status != Stale {
		err = statusError(r)
	}
	_, err = s.result(requests, changed, r, err)
	return err
}

func (s *Store) persist(requests map[string]Request) error {
	data, err := json.Marshal(snapshot{Version: 1, Requests: ordered(requests)})
	if err != nil {
		return err
	}
	if len(data) > maxFileSize {
		return errors.New("store exceeds size limit")
	}
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".approval-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncDirectory(d)
}

// Close releases the process lock; closing an already closed store is harmless.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(releaseLock(s.lock), s.lock.Close())
}

func lockError(err error) error {
	if err != nil {
		return fmt.Errorf("exclusive approval store lock: %w", err)
	}
	return nil
}
