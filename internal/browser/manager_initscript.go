package browser

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func (m *Manager) AddInitScript(ctx context.Context, opts InitScriptOptions) (InitScriptResult, error) {
	source, origin, err := NormalizeInitScript(opts)
	if err != nil {
		return InitScriptResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return InitScriptResult{}, err
	}
	defer cancel()

	existing := m.env.listInitScripts(tabID)
	if len(existing) >= maxInitScriptsPerTab {
		return InitScriptResult{}, fmt.Errorf("this tab already holds %d init scripts; remove one before adding another", maxInitScriptsPerTab)
	}
	wrapped := WrapInitScript(source, origin)
	var identifier string
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		id, err := page.AddScriptToEvaluateOnNewDocument(wrapped).WithRunImmediately(true).Do(runCtx)
		if err != nil {
			return err
		}
		identifier = string(id)
		return nil
	})); err != nil {
		return InitScriptResult{}, err
	}
	added := InitScript{
		ID:     identifier,
		Origin: origin,
		SHA256: initScriptDigest(source),
		Bytes:  len(source),
	}
	scripts := m.env.addInitScript(tabID, added)
	return InitScriptResult{
		OK:      true,
		TabID:   tabID,
		Added:   &added,
		Scripts: scripts,
		Count:   len(scripts),
		Message: "script will run at document-start on every later navigation of this tab, and was also run immediately in the document that is already open",
	}, nil
}

func (m *Manager) RemoveInitScript(ctx context.Context, opts InitScriptRemoveOptions) (InitScriptResult, error) {
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		return InitScriptResult{}, fmt.Errorf("id is required")
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return InitScriptResult{}, err
	}
	defer cancel()
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		return page.RemoveScriptToEvaluateOnNewDocument(page.ScriptIdentifier(id)).Do(runCtx)
	})); err != nil {
		return InitScriptResult{}, err
	}
	scripts, ok := m.env.removeInitScript(tabID, id)
	message := "removed the init script; later navigations of this tab will not run it. The document that is already open is unchanged"
	if !ok {
		message = "Chrome dropped the identifier; brw had no matching script recorded for this tab"
	}
	return InitScriptResult{OK: true, TabID: tabID, Removed: id, Scripts: scripts, Count: len(scripts), Message: message}, nil
}

func (m *Manager) ListInitScripts(ctx context.Context, tabID string) (InitScriptResult, error) {
	resolved, _, cancel, err := m.contextForTab(ctx, strings.TrimSpace(tabID))
	if err != nil {
		return InitScriptResult{}, err
	}
	cancel()
	scripts := m.env.listInitScripts(resolved)
	return InitScriptResult{OK: true, TabID: resolved, Scripts: scripts, Count: len(scripts)}, nil
}

func (e *environmentState) listInitScripts(tabID string) []InitScript {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	return append([]InitScript(nil), e.initScripts[tabID]...)
}

func (e *environmentState) addInitScript(tabID string, script InitScript) []InitScript {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	e.initScripts[tabID] = append(e.initScripts[tabID], script)
	return append([]InitScript(nil), e.initScripts[tabID]...)
}

func (e *environmentState) removeInitScript(tabID, id string) ([]InitScript, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	cur := e.initScripts[tabID]
	out := cur[:0]
	found := false
	for _, s := range cur {
		if s.ID == id {
			found = true
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		delete(e.initScripts, tabID)
		return nil, found
	}
	e.initScripts[tabID] = append([]InitScript(nil), out...)
	return append([]InitScript(nil), e.initScripts[tabID]...), found
}
