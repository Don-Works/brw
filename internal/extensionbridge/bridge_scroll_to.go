package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

// ScrollTo runs the shared scroll-into-view expression through the bridge's own
// Evaluate. The element-targeted scroll is a page action, not debugger-session
// state, so it works on the signed-in transport.
var _ browser.ScrollToController = (*Bridge)(nil)

func (b *Bridge) ScrollTo(ctx context.Context, target string) (browser.ActionResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return browser.ActionResult{}, fmt.Errorf("scroll target is required: pass a brw ref or a CSS selector, or use brw_scroll with a direction")
	}
	if err := browser.GuardCrossOriginRefs("scroll into view", browser.BridgeCrossOriginRemedy, target); err != nil {
		return browser.ActionResult{}, err
	}
	value, err := b.Evaluate(ctx, browser.ScrollToExpression(target))
	if err != nil {
		return browser.ActionResult{}, err
	}
	raw, _ := json.Marshal(value)
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Text  string `json:"text"`
	}
	_ = json.Unmarshal(raw, &out)
	if !out.OK {
		return browser.ActionResult{}, fmt.Errorf("scroll into view: %s", out.Error)
	}
	message := "scrolled into view target:" + target
	if out.Text != "" {
		message += " " + out.Text
	}
	return browser.ActionResult{OK: true, Message: message}, nil
}
