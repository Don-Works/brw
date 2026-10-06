package pagewatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/runlock"
	"github.com/Don-Works/brw/internal/siteconsent"
)

const (
	maxWatchers   = 64
	maxEvents     = 512
	maxStateBytes = 16 << 20
)

var watcherID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,96}$`)

type record struct {
	Watcher
	Digest      string    `json:"baseline_digest,omitempty"`
	Events      []Event   `json:"events"`
	NextSample  time.Time `json:"-"`
	NextRefresh time.Time `json:"-"`
	Unavailable bool      `json:"unavailable,omitempty"`
}

type diskState struct {
	Version  int               `json:"version"`
	Watchers map[string]record `json:"watchers"`
}

// Service owns durable watchers independently of disposable MCP clients.
type Service struct {
	mu         sync.Mutex
	controller browser.Controller
	policy     *navpolicy.Policy
	consent    *siteconsent.Guard
	path       string
	lock       *runlock.Lock
	records    map[string]record
	owned      sync.Map
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
}

// New loads bounded state and starts background sampling until Close.
func New(ctx context.Context, controller browser.Controller, root string, policy *navpolicy.Policy, consent *siteconsent.Guard) (*Service, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("page watcher root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("page watcher root must be a directory, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("page watcher root must be owner-only (0700)")
	}
	lock, err := runlock.Acquire(ctx, root, "service", 0)
	if err != nil {
		return nil, fmt.Errorf("page watcher store is already in use: %w", err)
	}
	s := &Service{controller: controller, policy: policy, consent: consent, path: filepath.Join(root, "watchers.json"), lock: lock, records: map[string]record{}, done: make(chan struct{})}
	if err := s.load(); err != nil {
		_ = lock.Release()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.run(runCtx)
	return s, nil
}

func normalize(opts RegisterOptions) (RegisterOptions, error) {
	if opts.ID != "" && !watcherID.MatchString(opts.ID) {
		return opts, errors.New("id must be 1..96 letters, digits, underscores or hyphens")
	}
	if strings.TrimSpace(opts.URL) == "" || len(opts.URL) > 4096 {
		return opts, errors.New("url is required and must be at most 4096 bytes")
	}
	normalized, err := navpolicy.NormalizeNavigationURL(opts.URL)
	if err != nil {
		return opts, err
	}
	u, err := url.Parse(normalized)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return opts, errors.New("page watchers require an http(s) URL without credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	opts.URL = u.String()
	if len(opts.Selector) > 2048 {
		return opts, errors.New("selector must be at most 2048 bytes")
	}
	if opts.Mode == "" {
		opts.Mode = "title"
		if opts.Selector != "" {
			opts.Mode = "text"
		}
	}
	switch opts.Mode {
	case "title":
		if opts.Selector != "" {
			return opts, errors.New("title mode does not take a selector")
		}
	case "text", "count":
		if opts.Selector == "" {
			return opts, errors.New("text and count modes require a selector")
		}
	default:
		return opts, errors.New("mode must be title, text or count")
	}
	if opts.IntervalMS == 0 {
		opts.IntervalMS = 5000
	}
	if opts.IntervalMS < 1000 || opts.IntervalMS > 300000 {
		return opts, errors.New("interval_ms must be between 1000 and 300000")
	}
	if opts.RefreshIntervalMS != 0 && (opts.RefreshIntervalMS < 5000 || opts.RefreshIntervalMS > 3600000) {
		return opts, errors.New("refresh_interval_ms must be 0 or between 5000 and 3600000")
	}
	return opts, nil
}

func (s *Service) authorize(rawURL string) error {
	if err := s.policy.Check(rawURL); err != nil {
		return err
	}
	return s.consent.AuthorizeUnattended(rawURL, siteconsent.ScopeRead)
}

// WatchPage registers an observation; a stable id makes retries idempotent.
func (s *Service) WatchPage(ctx context.Context, opts RegisterOptions) (Watcher, error) {
	opts, err := normalize(opts)
	if err != nil {
		return Watcher{}, err
	}
	if err := s.authorize(opts.URL); err != nil {
		return Watcher{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Watcher{}, errors.New("page watcher service is closed")
	}
	if err := ctx.Err(); err != nil {
		return Watcher{}, err
	}
	if old, ok := s.records[opts.ID]; ok {
		if old.RegisterOptions != opts {
			return Watcher{}, errors.New("watcher id already exists with different configuration; remove it before changing it")
		}
		return old.Watcher, nil
	}
	if len(s.records) >= maxWatchers {
		return Watcher{}, errors.New("page watcher limit of 64 reached; remove a watcher first")
	}
	if opts.ID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Watcher{}, err
		}
		opts.ID = "pw_" + hex.EncodeToString(id[:])
	}
	r := record{Watcher: Watcher{RegisterOptions: opts, Enabled: true, Status: "starting", CreatedAt: time.Now().UTC()}, Events: []Event{}}
	if err := s.store(opts.ID, &r); err != nil {
		return Watcher{}, err
	}
	return r.Watcher, nil
}

// PageWatchers changes durable registration state before acknowledging success.
func (s *Service) PageWatchers(ctx context.Context, opts ManageOptions) (ManageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ManageResult{}, errors.New("page watcher service is closed")
	}
	if err := ctx.Err(); err != nil {
		return ManageResult{}, err
	}
	action := opts.Action
	if action == "" {
		action = "list"
	}
	removed := false
	if action != "list" {
		if opts.ID == "" {
			return ManageResult{}, errors.New("id is required")
		}
		r, exists := s.records[opts.ID]
		switch action {
		case "remove":
			if exists {
				if err := s.closeTab(ctx, &r); err != nil {
					return ManageResult{}, err
				}
				if err := s.store(opts.ID, nil); err != nil {
					return ManageResult{}, err
				}
				s.owned.Delete(r.TabID)
				s.releaseOwnership(r.TabID)
				removed = true
			}
		case "pause", "resume":
			if !exists {
				return ManageResult{}, errors.New("page watcher not found")
			}
			previousTab := r.TabID
			if action == "resume" {
				if err := s.authorize(r.URL); err != nil {
					return ManageResult{}, err
				}
				if r.Status == "url_mismatch" {
					if err := s.closeTab(ctx, &r); err != nil {
						return ManageResult{}, err
					}
				}
				if r.Status == "tab_closed" || r.Status == "url_mismatch" {
					r.TabID = ""
				}
				r.Enabled = true
				r.Status = "starting"
				r.LastError = ""
				r.NextSample = time.Time{}
			} else {
				r.Enabled = false
				r.Status = "paused"
			}
			if err := s.store(opts.ID, &r); err != nil {
				return ManageResult{}, err
			}
			if previousTab != r.TabID {
				s.owned.Delete(previousTab)
				s.releaseOwnership(previousTab)
			}
		default:
			return ManageResult{}, errors.New("action must be list, remove, pause or resume")
		}
	}
	out := ManageResult{Watchers: make([]Watcher, 0, len(s.records)), Removed: removed}
	for _, r := range s.records {
		out.Watchers = append(out.Watchers, r.Watcher)
	}
	sort.Slice(out.Watchers, func(i, j int) bool { return out.Watchers[i].ID < out.Watchers[j].ID })
	return out, nil
}

// PageEvents reads a stable window without acknowledging or deleting events.
func (s *Service) PageEvents(ctx context.Context, opts EventsOptions) (EventsResult, error) {
	if opts.WatcherID == "" {
		return EventsResult{}, errors.New("watcher_id is required")
	}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.Limit < 1 || opts.Limit > 100 {
		return EventsResult{}, errors.New("limit must be between 1 and 100")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return EventsResult{}, err
	}
	r, ok := s.records[opts.WatcherID]
	if !ok {
		return EventsResult{}, errors.New("page watcher not found")
	}
	out := EventsResult{WatcherID: r.ID, Events: []Event{}, LatestSeq: r.Seq, OldestSeq: r.Seq + 1}
	if len(r.Events) > 0 {
		out.OldestSeq = r.Events[0].Seq
	}
	out.Gap = out.OldestSeq > 0 && opts.SinceSeq < out.OldestSeq-1
	for _, e := range r.Events {
		if e.Seq <= opts.SinceSeq {
			continue
		}
		if len(out.Events) >= opts.Limit {
			out.HasMore = true
			break
		}
		out.Events = append(out.Events, e)
	}
	return out, nil
}

// CheckTabAccess reserves owned watcher tabs against ordinary browser calls.
func (s *Service) CheckTabAccess(ctx context.Context, id string) error {
	if browser.IsBackgroundPage(ctx) {
		return nil
	}
	if _, ok := s.owned.Load(id); ok {
		return errors.New("tab belongs to a persistent page watcher; use brw_page_watchers to pause or remove it and brw_open for your own working tab")
	}
	return nil
}

// OwnsTab reports watcher tabs so list surfaces can mark them reserved.
func (s *Service) OwnsTab(id string) bool { _, ok := s.owned.Load(id); return ok }

func (s *Service) checkOwnership(ctx context.Context, id string) error {
	if owner, ok := s.controller.(browser.BackgroundTabOwner); ok {
		return owner.CheckBackgroundTab(ctx, id)
	}
	return nil
}

func (s *Service) releaseOwnership(id string) {
	if owner, ok := s.controller.(browser.BackgroundTabOwner); ok {
		owner.ReleaseBackgroundTab(id)
	}
}

func (s *Service) loseOwnership(id string, r *record) error {
	previousTab := r.TabID
	r.TabID = ""
	if err := s.store(id, r); err != nil {
		return err
	}
	s.owned.Delete(previousTab)
	s.releaseOwnership(previousTab)
	return nil
}

func (s *Service) closeTab(ctx context.Context, r *record) error {
	if r.TabID == "" {
		return nil
	}
	if err := s.checkOwnership(ctx, r.TabID); errors.Is(err, browser.ErrBackgroundOwnershipLost) {
		return nil
	} else if err != nil {
		return errors.New("cannot prove watcher tab ownership while browser is disconnected")
	}
	tabs, err := s.controller.ListTabs(ctx)
	if err != nil {
		return errors.New("cannot remove watcher while its browser is disconnected; pause it or reconnect first")
	}
	for _, tab := range tabs {
		if tab.ID != r.TabID {
			continue
		}
		if err := s.controller.CloseTab(browser.WithBackgroundPage(ctx), r.TabID); err != nil && !errors.Is(err, browser.ErrBackgroundOwnershipLost) {
			return errors.New("cannot close watcher tab; watcher remains registered")
		}
	}
	return nil
}

// Close cancels sampling, waits for it to finish, and releases the store lock.
func (s *Service) Close() error {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(browser.WithBackgroundPage(context.Background()), 5*time.Second)
	defer cancel()
	var cleanupErr error
	for _, r := range s.records {
		if r.TabID == "" || ctx.Err() != nil {
			continue
		}
		if err := s.closeTab(ctx, &r); err != nil {
			cleanupErr = err
		} else {
			s.owned.Delete(r.TabID)
			s.releaseOwnership(r.TabID)
		}
	}
	return errors.Join(cleanupErr, s.lock.Release())
}
