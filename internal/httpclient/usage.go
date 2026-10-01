package httpclient

import (
	"context"
	"github.com/Don-Works/brw/internal/usagelog"
	"time"
)

func (c *Controller) ReportUsage(ctx context.Context, event usagelog.Event) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
	defer cancel()
	return c.postExactWithLimit(ctx, "/api/usage/report", event, nil, 1024)
}
