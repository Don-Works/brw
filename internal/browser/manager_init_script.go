package browser

import (
	"context"
	"fmt"
	"strings"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// InitScript adds, removes, lists or clears the JavaScript registered to run
// before every new document in a tab.
//
// The identifier Chrome returns from addScriptToEvaluateOnNewDocument is scoped
// to the debugger session, so this is direct-CDP only; the extension bridge
// attaches and detaches per operation and would drop the registration. The
// capability error is raised upstream by the transport, and the MCP layer hides
// the tool on lanes that cannot hold it.
func (m *Manager) InitScript(ctx context.Context, opts InitScriptOptions) (InitScriptResult, error) {
	req, err := NormalizeInitScript(opts)
	if err != nil {
		return InitScriptResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(req.TabID))
	if err != nil {
		return InitScriptResult{}, err
	}
	defer cancel()

	switch req.Action {
	case "add":
		var identifier cdppage.ScriptIdentifier
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			id, err := cdppage.AddScriptToEvaluateOnNewDocument(req.Source).Do(runCtx)
			identifier = id
			return err
		})); err != nil {
			return InitScriptResult{}, fmt.Errorf("register init script: %w", err)
		}
		script := NewInitScript(string(identifier), req.Source)
		m.env.addInitScript(tabID, script)
		return InitScriptResult{
			OK:      true,
			TabID:   tabID,
			Action:  "add",
			Added:   &script,
			Scripts: m.env.listInitScripts(tabID),
			Message: "registered; it runs before the document's own scripts on every new document in this tab, including the next navigation",
		}, nil

	case "remove":
		script, ok := m.env.removeInitScript(tabID, req.ID)
		if !ok {
			return InitScriptResult{}, fmt.Errorf("no init script %q is registered on this tab; list them with action=list", req.ID)
		}
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return cdppage.RemoveScriptToEvaluateOnNewDocument(cdppage.ScriptIdentifier(script.ID)).Do(runCtx)
		})); err != nil {
			// Chrome refused, so the registration is still live: put the record
			// back rather than forgetting a script that will run again.
			m.env.addInitScript(tabID, script)
			return InitScriptResult{}, fmt.Errorf("remove init script: %w", err)
		}
		return InitScriptResult{
			OK:      true,
			TabID:   tabID,
			Action:  "remove",
			Removed: script.ID,
			Scripts: m.env.listInitScripts(tabID),
			Message: "removed; documents already loaded keep whatever the script did to them",
		}, nil

	case "clear":
		scripts := m.env.clearInitScripts(tabID)
		var failed []string
		for _, script := range scripts {
			if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
				return cdppage.RemoveScriptToEvaluateOnNewDocument(cdppage.ScriptIdentifier(script.ID)).Do(runCtx)
			})); err != nil {
				// A session-scoped identifier can already be gone after a
				// navigation or reconnect; that is not a reason to keep it in
				// the registry, but it is worth reporting.
				failed = append(failed, script.ID)
			}
		}
		result := InitScriptResult{
			OK:      true,
			TabID:   tabID,
			Action:  "clear",
			Scripts: m.env.listInitScripts(tabID),
			Message: fmt.Sprintf("cleared %d init script(s)", len(scripts)),
		}
		if len(failed) > 0 {
			result.Message += fmt.Sprintf("; %d could not be removed from Chrome (already gone): %s", len(failed), strings.Join(failed, ", "))
		}
		return result, nil

	default: // list
		return InitScriptResult{
			OK:      true,
			TabID:   tabID,
			Action:  "list",
			Scripts: m.env.listInitScripts(tabID),
			Message: "scripts registered to run before each new document in this tab",
		}, nil
	}
}
