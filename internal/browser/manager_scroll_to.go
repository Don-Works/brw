package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// ScrollToController is the optional capability for bringing one element into
// view. It is separate from Controller so a lane without it errors by name.
type ScrollToController interface {
	ScrollTo(context.Context, string) (ActionResult, error)
}

// ScrollToExpression builds the in-page expression that scrolls one element into
// view. Exported so the extension bridge can run the same walker.
func ScrollToExpression(target string) string {
	quoted, _ := json.Marshal(target)
	return `(function (target) {` + snapshot.FrameWalkHelpers + `
  var el = null;
  var hit = __abFindDeep(target);
  if (hit) el = hit.el;
  if (!el) { try { el = document.querySelector(target); } catch (e2) { el = null; } }
  if (!el) return { ok: false, error: 'no element matches ' + JSON.stringify(target) };
  try { el.scrollIntoView({ block: 'center', inline: 'nearest' }); } catch (e3) { return { ok: false, error: String(e3 && e3.message || e3) }; }
  return { ok: true, tag: (el.tagName || '').toLowerCase(), text: String(el.innerText || '').replace(/\s+/g, ' ').trim().slice(0, 80) };
})(` + string(quoted) + `)`
}

// ScrollTo brings one element into view by ref or CSS selector, which is the
// scroll an agent usually wants: "show me this", not "move 500px". It is the
// element-targeted counterpart to Scroll's direction.
func (m *Manager) ScrollTo(ctx context.Context, target string) (ActionResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return ActionResult{}, fmt.Errorf("scroll target is required: pass a brw ref or a CSS selector, or use brw_scroll with a direction")
	}
	if err := m.guardTakeover("scroll_to"); err != nil {
		return ActionResult{}, err
	}
	if err := GuardCrossOriginRefs("scroll into view", GenericCrossOriginRemedy, target); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Tag   string `json:"tag"`
		Text  string `json:"text"`
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return chromedp.Run(tabCtx, chromedp.Evaluate(ScrollToExpression(target), &out))
	}); err != nil {
		return ActionResult{}, err
	}
	if !out.OK {
		return ActionResult{}, fmt.Errorf("scroll into view: %s", out.Error)
	}
	desc := fmt.Sprintf("scrolled into view target:%s", target)
	if out.Text != "" {
		desc += " " + out.Text
	}
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(tabID, TraceEntry{
		Action:     "scroll_to",
		Ref:        target,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}
