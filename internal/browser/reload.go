package browser

import (
	"context"

	"github.com/chromedp/chromedp"
)

// PageReloader refreshes a pinned document without a post-action content read.
type PageReloader interface{ ReloadPage(context.Context) error }

// ReloadPage waits for a replacement document without sampling its content.
func (m *Manager) ReloadPage(ctx context.Context) error {
	if err := m.guardTakeover("reload"); err != nil {
		return err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := chromedp.Run(tabCtx, chromedp.Reload()); err != nil {
		return err
	}
	return m.guardCurrentURL(tabID, tabCtx)
}
