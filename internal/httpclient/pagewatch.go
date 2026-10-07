package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/pagewatch"
)

var _ pagewatch.API = (*Controller)(nil)

// WatchPage registers on the persistent browser host, never on this proxy.
func (c *Controller) WatchPage(ctx context.Context, opts pagewatch.RegisterOptions) (pagewatch.Watcher, error) {
	var out pagewatch.Watcher
	err := c.post(ctx, "/api/watchers/register", opts, &out)
	return out, err
}

// PageWatchers manages registrations independently of this proxy's tab leases.
func (c *Controller) PageWatchers(ctx context.Context, opts pagewatch.ManageOptions) (pagewatch.ManageResult, error) {
	var out pagewatch.ManageResult
	err := c.post(ctx, "/api/watchers/manage", opts, &out)
	return out, err
}

// PageEvents reads durable browser-host event metadata after an opaque cursor.
func (c *Controller) PageEvents(ctx context.Context, opts pagewatch.EventsOptions) (pagewatch.EventsResult, error) {
	var out pagewatch.EventsResult
	err := c.post(ctx, "/api/watchers/events", opts, &out)
	return out, err
}
