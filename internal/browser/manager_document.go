package browser

import (
	"context"
	"errors"
	"strings"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// DocumentIdentity returns the exact committed main-frame document currently
// bound to the target. Chrome's loaderId changes for every replacement
// document but remains stable for same-document history updates, which makes it
// a stronger capture boundary than URL or origin comparisons.
func (m *Manager) DocumentIdentity(ctx context.Context) (DocumentIdentity, error) {
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return DocumentIdentity{}, err
	}
	defer cancel()
	persistentTabCtx, err := m.tabContext(tabID)
	if err != nil {
		return DocumentIdentity{}, err
	}
	if err := m.ensureDocumentTracking(tabID, persistentTabCtx, tabCtx); err != nil {
		return DocumentIdentity{}, err
	}

	var tree *page.FrameTree
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(cdpCtx context.Context) error {
		var getErr error
		tree, getErr = page.GetFrameTree().Do(cdpCtx)
		return getErr
	})); err != nil {
		return DocumentIdentity{}, err
	}
	if tree == nil || tree.Frame == nil {
		return DocumentIdentity{}, errors.New("main-document identity is unavailable")
	}
	frameID := strings.TrimSpace(tree.Frame.ID.String())
	loaderID := strings.TrimSpace(tree.Frame.LoaderID.String())
	origin := strings.TrimSpace(tree.Frame.SecurityOrigin)
	if tabID == "" || frameID == "" || loaderID == "" || origin == "" || origin == "null" {
		return DocumentIdentity{}, errors.New("main-document identity is unavailable")
	}
	if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
		return DocumentIdentity{}, err
	}
	m.documentMu.Lock()
	epoch := m.documentEpoch[tabID]
	m.documentMu.Unlock()
	return DocumentIdentity{
		ID:     tabID + "\x00" + frameID + "\x00" + loaderID + "\x00" + formatDocumentEpoch(epoch),
		Origin: origin,
	}, nil
}

func (m *Manager) ensureDocumentTracking(tabID string, persistentTabCtx, operationCtx context.Context) error {
	m.documentMu.Lock()
	if !m.documentTracked[tabID] {
		// Register before Page.enable so a commit in the enable/query window cannot
		// slip past the monotonic epoch. A listener lives with the persistent tab
		// context rather than this call's timeout context.
		chromedp.ListenTarget(persistentTabCtx, func(event any) {
			var frameIsMain bool
			switch typed := event.(type) {
			case *page.EventFrameNavigated:
				frameIsMain = typed.Frame != nil && typed.Frame.ParentID == ""
			case *page.EventDocumentOpened:
				frameIsMain = typed.Frame != nil && typed.Frame.ParentID == ""
			default:
				return
			}
			if frameIsMain {
				m.documentMu.Lock()
				// A target-destroyed callback may race one final queued Page event.
				// Do not recreate per-tab state after forgetTab removed it.
				if m.documentTracked[tabID] {
					m.documentEpoch[tabID]++
				}
				m.documentMu.Unlock()
			}
		})
		m.documentTracked[tabID] = true
	}
	ready := m.documentReady[tabID]
	m.documentMu.Unlock()
	if ready {
		return nil
	}
	if err := chromedp.Run(operationCtx, page.Enable()); err != nil {
		return err
	}
	m.documentMu.Lock()
	m.documentReady[tabID] = true
	m.documentMu.Unlock()
	return nil
}

func formatDocumentEpoch(epoch uint64) string {
	// Avoid fmt in this hot path and keep the composite identity opaque.
	const digits = "0123456789abcdef"
	if epoch == 0 {
		return "0"
	}
	var buf [16]byte
	index := len(buf)
	for epoch > 0 {
		index--
		buf[index] = digits[epoch&0xf]
		epoch >>= 4
	}
	return string(buf[index:])
}
