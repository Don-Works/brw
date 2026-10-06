package extensionbridge

import (
	"context"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// ReloadPage waits for a replacement document without reading its contents.
func (b *Bridge) ReloadPage(ctx context.Context) error {
	id := b.contextTabID(ctx)
	before, err := b.mainFrameState(ctx, id)
	if err != nil {
		return err
	}
	if err := b.navigateDirection(ctx, navigateReload); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, navigationCommitTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		after, err := b.mainFrameState(waitCtx, id)
		if err == nil && after.LoaderID != "" && after.LoaderID != before.LoaderID {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return errors.New("watcher refresh did not commit a replacement document")
		case <-ticker.C:
		}
	}
}

// CheckBackgroundTab binds a private tab to the socket that created it.
func (b *Bridge) CheckBackgroundTab(ctx context.Context, id string) error {
	conn, err := b.getConn(ctx)
	if err != nil {
		return err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.conn != conn || b.backgroundTabs[id] != conn {
		return browser.ErrBackgroundOwnershipLost
	}
	return nil
}

// ReleaseBackgroundTab forgets an ended watcher claim.
func (b *Bridge) ReleaseBackgroundTab(id string) {
	b.mu.Lock()
	delete(b.backgroundTabs, id)
	b.mu.Unlock()
}
