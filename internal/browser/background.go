package browser

import (
	"context"
	"errors"

	"github.com/chromedp/chromedp"
)

type backgroundPageKey struct{}

// ErrBackgroundOwnershipLost refuses a tab whose creating connection no longer exists.
var ErrBackgroundOwnershipLost = errors.New("watcher tab ownership belongs to an earlier browser connection")

// BackgroundTabOwner proves ownership before sampling, refreshing or closing a tab.
type BackgroundTabOwner interface {
	CheckBackgroundTab(context.Context, string) error
	ReleaseBackgroundTab(string)
}

// ReleaseBackgroundTab forgets an ended watcher claim.
func (m *Manager) ReleaseBackgroundTab(id string) { m.backgroundTabs.Delete(id) }

// CheckBackgroundTab binds a tab claim to this manager's browser connection.
func (m *Manager) CheckBackgroundTab(_ context.Context, id string) error {
	if _, ok := m.backgroundTabs.Load(id); !ok {
		return ErrBackgroundOwnershipLost
	}
	state := chromedp.FromContext(m.browserCtx)
	if state == nil || state.Browser == nil {
		return ErrBackgroundOwnershipLost
	}
	select {
	case <-state.Browser.LostConnection:
		return ErrBackgroundOwnershipLost
	default:
		return nil
	}
}

// WithBackgroundPage lets the browser-host watcher service open and read its
// private tabs without changing the caller's default tab or foreground window.
func WithBackgroundPage(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundPageKey{}, true)
}

// IsBackgroundPage identifies the watcher service's private browser operations.
func IsBackgroundPage(ctx context.Context) bool {
	value, _ := ctx.Value(backgroundPageKey{}).(bool)
	return value
}

// SetTabAccessGuard installs the browser host's private-tab boundary.
func (m *Manager) SetTabAccessGuard(check func(context.Context, string) error) {
	m.tabAccessGuard = check
}

func (m *Manager) checkTabAccess(ctx context.Context, id string) error {
	if IsBackgroundPage(ctx) && id != "" {
		if err := m.CheckBackgroundTab(ctx, id); err != nil {
			return err
		}
	}
	if m.tabAccessGuard != nil {
		return m.tabAccessGuard(ctx, id)
	}
	return nil
}
