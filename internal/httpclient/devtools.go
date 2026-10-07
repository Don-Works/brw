package httpclient

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

const auditClientFloor = 120 * time.Second

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
