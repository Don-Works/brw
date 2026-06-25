package browser

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
)

// OpenIncognito creates a fresh, fully isolated incognito browser context — its
// own cookies, storage, and cache, sharing nothing with the default profile or
// any other context — and opens url in a new tab inside it. Direct-CDP
// transport only: it uses CDP Target.createBrowserContext, the same mechanism
// Puppeteer exposes as an "incognito browser context". The returned Tab carries
// the BrowserContextID; pass it to CloseContext to dispose the whole context
// (closing every tab in it and discarding its data) when the throwaway session
// is done.
func (m *Manager) OpenIncognito(ctx context.Context, url string) (OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}

	var ctxID cdp.BrowserContextID
	var id target.ID
	if err := m.runBrowser(ctx, func(ctx context.Context) error {
		var err error
		ctxID, err = target.CreateBrowserContext().Do(ctx)
		if err != nil {
			return err
		}
		// A fresh browser context has no window yet, so create the first target
		// with NewWindow — otherwise Chrome reports "no browser is open". This also
		// gives the incognito session its own window, matching incognito semantics.
		id, err = target.CreateTarget(url).WithBrowserContextID(ctxID).WithNewWindow(true).Do(ctx)
		return err
	}); err != nil {
		return OpenResult{}, err
	}

	m.trackIncognito(string(ctxID))

	tabID := string(id)
	m.refs.SetActive(tabID)
	ready := m.WaitFor(ctx, "load", 10*time.Second) == nil
	// As with Open, do NOT OS-activate the tab; foreground focus stays reserved
	// for the explicit FocusTab tool.
	tab, err := m.tabByID(ctx, tabID)
	if err != nil {
		tab = Tab{ID: tabID, URL: url, Type: "page"}
	}
	tab.BrowserContextID = string(ctxID)
	return OpenResult{Tab: tab, Ready: ready}, nil
}

// CloseContext disposes an incognito browser context created by OpenIncognito,
// closing every tab inside it and discarding its isolated cookies/storage. The
// id is the BrowserContextID returned by OpenIncognito. Direct-CDP only.
func (m *Manager) CloseContext(ctx context.Context, contextID string) error {
	contextID = strings.TrimSpace(contextID)
	if contextID == "" {
		return errors.New("context_id is required")
	}
	err := m.runBrowser(ctx, func(ctx context.Context) error {
		return target.DisposeBrowserContext(cdp.BrowserContextID(contextID)).Do(ctx)
	})
	if err == nil {
		m.untrackIncognito(contextID)
	}
	return err
}

// trackIncognito / untrackIncognito / takeIncognitoContexts are the bookkeeping
// for incognito BrowserContexts so Close can dispose any the caller forgot.
// Kept as tiny helpers (no browser I/O) so the leak-cleanup logic is unit
// testable independent of a live Chrome.
func (m *Manager) trackIncognito(contextID string) {
	m.incognitoMu.Lock()
	m.incognitoContexts[contextID] = true
	m.incognitoMu.Unlock()
}

func (m *Manager) untrackIncognito(contextID string) {
	m.incognitoMu.Lock()
	delete(m.incognitoContexts, contextID)
	m.incognitoMu.Unlock()
}

func (m *Manager) takeIncognitoContexts() []string {
	m.incognitoMu.Lock()
	ids := make([]string, 0, len(m.incognitoContexts))
	for id := range m.incognitoContexts {
		ids = append(ids, id)
	}
	m.incognitoContexts = map[string]bool{}
	m.incognitoMu.Unlock()
	return ids
}

// disposeIncognitoContexts disposes every incognito BrowserContext still tracked
// (i.e. opened via OpenIncognito but never CloseContext'd), best-effort, while
// the browser is still alive. Called from Manager.Close so a forgetful caller
// doesn't leak isolated contexts. Each disposal is bounded by its own timeout so
// a wedged context cannot stall shutdown.
func (m *Manager) disposeIncognitoContexts() {
	for _, id := range m.takeIncognitoContexts() {
		disposeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.runBrowser(disposeCtx, func(ctx context.Context) error {
			return target.DisposeBrowserContext(cdp.BrowserContextID(id)).Do(ctx)
		})
		cancel()
	}
}
