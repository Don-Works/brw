package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"

	"github.com/Don-Works/brw/internal/sessionstate"
)

// Session-state actions. There is deliberately no "read" or "export": see
// docs/auth-model.md for why the absence of that action is the whole security
// argument.
const (
	SessionStateActionSave    = "save"
	SessionStateActionRestore = "restore"
	SessionStateActionList    = "list"
	SessionStateActionDelete  = "delete"
)

// ErrSessionStateDisabled is returned when the daemon has no snapshot store.
// The store refuses to exist without an at-rest key, so "disabled" and
// "unencrypted" are the same state and neither writes a snapshot.
var ErrSessionStateDisabled = errors.New("session snapshots are not enabled on this daemon: start brwd with --state-key-file pointing at an owner-only key file of at least 32 bytes")

// ErrSessionStateEmpty refuses to seal a snapshot that would restore nothing.
// An empty snapshot is always a mistake — the wrong origins, or a context that
// was never signed in — and saving one hides it until the restore that fails.
var ErrSessionStateEmpty = errors.New("no cookies in this browser context matched the named origins, so there is nothing to seal; sign in first, or check the origins")

// SessionStateOptions is the shared request shape across transports.
type SessionStateOptions struct {
	Action     string   `json:"action"`
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Origins    []string `json:"origins,omitempty"`
	// Redact drops cookie NAMES matching a glob, on top of the origin
	// allowlist. It can only narrow what is sealed.
	Redact []string `json:"redact,omitempty"`
	// TTLSeconds may shorten the store's retention for one snapshot, never
	// lengthen it.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// ContextID names the incognito browser context to read from or write to.
	// Empty means the profile's default context.
	ContextID string `json:"context_id,omitempty"`
}

// SessionStateResult is the metadata-only answer. Every field here is safe to
// put in a model context, a log line or an HTTP response: none of them can
// carry a cookie name or value.
type SessionStateResult struct {
	Action    string              `json:"action"`
	ContextID string              `json:"context_id,omitempty"`
	Snapshot  *sessionstate.Meta  `json:"snapshot,omitempty"`
	Snapshots []sessionstate.Meta `json:"snapshots,omitempty"`
	// RestoredCookies is how many cookies the restore put into the browser.
	RestoredCookies int `json:"restored_cookies,omitempty"`
	// SkippedOffAllowlist counts cookies the origin allowlist refused. On a
	// restore a non-zero value means the snapshot carried material for an
	// origin this caller did not ask for, and it was not applied.
	SkippedOffAllowlist int    `json:"skipped_off_allowlist,omitempty"`
	Note                string `json:"note,omitempty"`
}

// Validate checks the action-specific required fields before any browser I/O,
// so every transport refuses a malformed request the same way.
func (o SessionStateOptions) Validate() error {
	switch o.action() {
	case SessionStateActionSave:
		if len(o.Origins) == 0 {
			return sessionstate.ErrNoOrigins
		}
	case SessionStateActionRestore, SessionStateActionDelete:
		if strings.TrimSpace(o.SnapshotID) == "" {
			return fmt.Errorf("snapshot_id is required for %s", o.action())
		}
	case SessionStateActionList:
	case "":
		return errors.New(`action is required: "save", "restore", "list", or "delete"`)
	default:
		return fmt.Errorf("unknown action %q: want save, restore, list, or delete", o.Action)
	}
	if o.TTLSeconds < 0 {
		return errors.New("ttl_seconds must not be negative")
	}
	return nil
}

func (o SessionStateOptions) action() string {
	return strings.ToLower(strings.TrimSpace(o.Action))
}

// SetSessionStateStore installs the browser host's snapshot store. A nil store
// leaves the capability advertised but refusing by name, which is the honest
// answer for a daemon started without a key.
func (m *Manager) SetSessionStateStore(store *sessionstate.Store) {
	m.sessionStateMu.Lock()
	defer m.sessionStateMu.Unlock()
	m.sessionState = store
}

func (m *Manager) sessionStateStore() *sessionstate.Store {
	m.sessionStateMu.Lock()
	defer m.sessionStateMu.Unlock()
	return m.sessionState
}

// SessionState implements brw_state on the direct-CDP transport. Cookies are
// read and written at the BROWSER context level (CDP Storage.getCookies /
// Storage.setCookies with a browserContextId), so an incognito context created
// by OpenIncognito is addressable by its own id and needs no open tab.
func (m *Manager) SessionState(ctx context.Context, opts SessionStateOptions) (SessionStateResult, error) {
	if err := opts.Validate(); err != nil {
		return SessionStateResult{}, err
	}
	store := m.sessionStateStore()
	if store == nil {
		return SessionStateResult{}, ErrSessionStateDisabled
	}
	action := opts.action()
	switch action {
	case SessionStateActionList:
		metas, err := store.List()
		if err != nil {
			return SessionStateResult{}, err
		}
		if metas == nil {
			metas = []sessionstate.Meta{}
		}
		return SessionStateResult{Action: action, Snapshots: metas}, nil

	case SessionStateActionDelete:
		if err := store.Delete(strings.TrimSpace(opts.SnapshotID)); err != nil {
			return SessionStateResult{}, err
		}
		return SessionStateResult{Action: action, Note: "snapshot deleted"}, nil

	case SessionStateActionSave:
		allow, err := sessionstate.ParseAllowlist(opts.Origins)
		if err != nil {
			return SessionStateResult{}, err
		}
		cookies, err := m.contextCookies(ctx, opts.ContextID)
		if err != nil {
			return SessionStateResult{}, err
		}
		kept, skipped, err := sessionstate.Restrict(cookies, allow, opts.Redact)
		if err != nil {
			return SessionStateResult{}, err
		}
		if len(kept) == 0 {
			return SessionStateResult{}, ErrSessionStateEmpty
		}
		meta, err := store.Save(sessionstate.Snapshot{
			Origins: allow.Origins(),
			Cookies: kept,
		}, sessionstate.SaveOptions{TTL: time.Duration(opts.TTLSeconds) * time.Second})
		if err != nil {
			return SessionStateResult{}, err
		}
		return SessionStateResult{
			Action:              action,
			ContextID:           strings.TrimSpace(opts.ContextID),
			Snapshot:            &meta,
			SkippedOffAllowlist: skipped,
		}, nil

	case SessionStateActionRestore:
		snapshot, meta, err := store.Load(strings.TrimSpace(opts.SnapshotID))
		if err != nil {
			return SessionStateResult{}, err
		}
		// The allowlist is applied AGAIN here, against the origins this caller
		// named (or, absent those, the ones the snapshot was sealed with). A
		// snapshot that carries a cookie for some other origin therefore cannot
		// put it into a browser — the filter is on the restore side, not only
		// the capture side.
		requested := opts.Origins
		if len(requested) == 0 {
			requested = snapshot.Origins
		}
		allow, err := sessionstate.ParseAllowlist(requested)
		if err != nil {
			return SessionStateResult{}, err
		}
		kept, skipped, err := sessionstate.Restrict(snapshot.Cookies, allow, nil)
		if err != nil {
			return SessionStateResult{}, err
		}
		if len(kept) == 0 {
			return SessionStateResult{}, fmt.Errorf("snapshot %s carries no cookie for the requested origins, so nothing was restored", meta.ID)
		}
		if err := m.applyCookies(ctx, opts.ContextID, kept); err != nil {
			return SessionStateResult{}, err
		}
		return SessionStateResult{
			Action:              action,
			ContextID:           strings.TrimSpace(opts.ContextID),
			Snapshot:            &meta,
			RestoredCookies:     len(kept),
			SkippedOffAllowlist: skipped,
		}, nil
	}
	return SessionStateResult{}, fmt.Errorf("unknown action %q", opts.Action)
}

// contextCookies reads every cookie in one browser context. Storage.getCookies
// is a browser-level command, so it works on a freshly created incognito
// context that has no tab of its own yet.
func (m *Manager) contextCookies(ctx context.Context, contextID string) ([]sessionstate.Cookie, error) {
	var raw []*network.Cookie
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		cmd := storage.GetCookies()
		if id := strings.TrimSpace(contextID); id != "" {
			cmd = cmd.WithBrowserContextID(cdp.BrowserContextID(id))
		}
		var err error
		raw, err = cmd.Do(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("Storage.getCookies: %w", err)
	}
	out := make([]sessionstate.Cookie, 0, len(raw))
	for _, cookie := range raw {
		if cookie == nil {
			continue
		}
		out = append(out, sessionstate.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Expires:  cookie.Expires,
			Secure:   cookie.Secure,
			HTTPOnly: cookie.HTTPOnly,
			SameSite: string(cookie.SameSite),
		})
	}
	return out, nil
}

func (m *Manager) applyCookies(ctx context.Context, contextID string, cookies []sessionstate.Cookie) error {
	params := make([]*network.CookieParam, 0, len(cookies))
	for _, cookie := range cookies {
		param := &network.CookieParam{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Secure:   cookie.Secure,
			HTTPOnly: cookie.HTTPOnly,
		}
		if cookie.SameSite != "" {
			param.SameSite = network.CookieSameSite(cookie.SameSite)
		}
		if cookie.Expires > 0 {
			expires := cdp.TimeSinceEpoch(time.Unix(int64(cookie.Expires), 0))
			param.Expires = &expires
		}
		params = append(params, param)
	}
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		cmd := storage.SetCookies(params)
		if id := strings.TrimSpace(contextID); id != "" {
			cmd = cmd.WithBrowserContextID(cdp.BrowserContextID(id))
		}
		return cmd.Do(ctx)
	}); err != nil {
		return fmt.Errorf("Storage.setCookies: %w", err)
	}
	return nil
}

var _ SessionStateController = (*Manager)(nil)
