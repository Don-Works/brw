package httpclient

import (
	"context"
	"errors"
	"github.com/Don-Works/brw/internal/usagelog"
	"time"
)

func (c *Controller) ReportUsage(ctx context.Context, event usagelog.Event) error {
	if c == nil || c.client == nil || c.baseURL == "" {
		return errors.New("usage reporting controller is unconfigured")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
	defer cancel()
	return c.postExactWithLimit(ctx, "/api/usage/report", event, nil, 1024)
}
