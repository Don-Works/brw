package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Check runs the shared checkbox expression through the bridge's own Evaluate.
// Setting a checkbox is a page action, not debugger-session state, so it works
// on the signed-in transport.
var _ browser.CheckController = (*Bridge)(nil)

func (b *Bridge) Check(ctx context.Context, opts browser.CheckOptions) (browser.ActionResult, error) {
	req, err := browser.NormalizeCheck(opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("check", browser.BridgeCrossOriginRemedy, req.Ref); err != nil {
		return browser.ActionResult{}, err
	}
	target := req.Ref
	if target == "" {
		matches, findErr := b.Find(ctx, snapshot.FindOptions{Query: req.Query, Role: req.Role, Limit: 2})
		if findErr != nil {
			return browser.ActionResult{}, fmt.Errorf("locate checkbox: %w", findErr)
		}
		if len(matches.Elements) == 0 {
			return browser.ActionResult{}, fmt.Errorf("no element matches query %q", req.Query)
		}
		if len(matches.Elements) > 1 {
			return browser.ActionResult{}, fmt.Errorf("query %q matched %d elements; pass a ref", req.Query, len(matches.Elements))
		}
		target = matches.Elements[0].Ref
	}
	if err := browser.GuardCrossOriginRefs("check", browser.BridgeCrossOriginRemedy, target); err != nil {
		return browser.ActionResult{}, err
	}
	value, err := b.Evaluate(ctx, browser.CheckExpression(target, *req.Checked))
	if err != nil {
		return browser.ActionResult{}, err
	}
	raw, _ := json.Marshal(value)
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Checked bool   `json:"checked"`
		Changed bool   `json:"changed"`
	}
	_ = json.Unmarshal(raw, &out)
	if !out.OK {
		return browser.ActionResult{}, fmt.Errorf("check %s: %s", target, out.Error)
	}
	message := fmt.Sprintf("set checked=%t on %s", out.Checked, target)
	if out.Changed {
		message = fmt.Sprintf("changed checked to %t on %s", out.Checked, target)
	}
	return browser.ActionResult{OK: true, Message: message}, nil
}
