package httpclient

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

// auditClientFloor keeps the proxy's HTTP deadline clear of the audit the
// browser host is running. axe walks every node against every rule, so on a
// large application the answer legitimately takes longer than any interaction
// the default client timeout was sized for.
const auditClientFloor = 120 * time.Second

// The proxy forwards the developer observations to the daemon that owns the
// browser. The accessibility report is stored on that side and only its handle
// comes back, which is the data-locality rule the artifact capture path already
// follows: no page payload crosses into the disposable MCP process.
//
// It has to satisfy the capability itself, or an upstream process would answer
// "this transport cannot" for a daemon that can.
var _ devtools.Observer = (*Controller)(nil)

func (c *Controller) Vitals(ctx context.Context, opts devtools.VitalsOptions) (devtools.Vitals, error) {
	opts = opts.Normalize()
	var out devtools.Vitals
	client := withMinimumTimeout(c.client, time.Duration(opts.SettleMS)*time.Millisecond+30*time.Second)
	err := c.postWithClient(ctx, client, "/api/page/vitals", opts, &out)
	return out, err
}

func (c *Controller) AccessibilityAudit(ctx context.Context, opts devtools.AuditOptions) (devtools.AuditResult, error) {
	var out devtools.AuditResult
	client := withMinimumTimeout(c.client, auditClientFloor)
	err := c.postWithClient(ctx, client, "/api/page/a11y", opts, &out)
	return out, err
}

func (c *Controller) Highlight(ctx context.Context, opts devtools.HighlightOptions) (devtools.HighlightResult, error) {
	var out devtools.HighlightResult
	err := c.post(ctx, "/api/page/highlight", opts, &out)
	return out, err
}
