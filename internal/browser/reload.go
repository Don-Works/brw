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
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Run(tabCtx, chromedp.Reload())
}
