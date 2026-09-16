package extensionbridge

import (
	"context"
	"encoding/json"

	"github.com/Don-Works/brw/internal/browser"
)

// React runs the shared fiber-walker expression through the bridge's own
// Evaluate. React introspection is a page read, not debugger-session state, so
// unlike the environment overrides it works on the signed-in transport too.
var _ browser.ReactController = (*Bridge)(nil)

func (b *Bridge) React(ctx context.Context, opts browser.ReactOptions) (browser.ReactResult, error) {
	req, err := browser.NormalizeReact(opts)
	if err != nil {
		return browser.ReactResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("react introspect", browser.BridgeCrossOriginRemedy, req.Target); err != nil {
		return browser.ReactResult{}, err
	}
	value, err := b.Evaluate(ctx, browser.ReactExpression(req))
	if err != nil {
		return browser.ReactResult{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return browser.ReactResult{}, err
	}
	var out browser.ReactResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return browser.ReactResult{}, err
	}
	out.Action = req.Action
	return out, nil
}
