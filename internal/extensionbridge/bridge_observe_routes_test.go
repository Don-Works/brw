package extensionbridge

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// brw_route's description promises that active routes are reported by
// brw_observe "so mocked traffic is never invisible in the transcript", and a
// promise in a tool description is not about one transport. This bridge cannot
// replay a HAR — it refuses that by name — but it does install abort rules, and
// a declarativeNetRequest rule silently eating a request reads in a transcript
// exactly like a site being down.
func TestBridgeObserveReportsTheTabsActiveRoutes(t *testing.T) {
	b, _, cleanup := connectPropertyWriteFake(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "7"), 5*time.Second)
	defer cancel()

	before, err := b.Observe(ctx)
	if err != nil {
		t.Fatalf("observe before routing: %v", err)
	}
	if before.ActiveRoutes != 0 {
		t.Fatalf("active_routes = %d on an unrouted tab, want 0", before.ActiveRoutes)
	}

	for _, pattern := range []string{"https://analytics.test/*", "https://ads.test/*"} {
		if _, err := b.Route(ctx, browser.RouteOptions{
			Action: "add", TabID: "7", Pattern: pattern, Behaviour: string(browser.RouteAbort),
		}); err != nil {
			t.Fatalf("add route %s: %v", pattern, err)
		}
	}

	after, err := b.Observe(ctx)
	if err != nil {
		t.Fatalf("observe after routing: %v", err)
	}
	if after.ActiveRoutes != 2 {
		t.Fatalf("active_routes = %d, want the 2 installed rules; mocked traffic is invisible in the transcript", after.ActiveRoutes)
	}

	if _, err := b.Route(ctx, browser.RouteOptions{Action: "clear", TabID: "7"}); err != nil {
		t.Fatalf("clear routes: %v", err)
	}
	cleared, err := b.Observe(ctx)
	if err != nil {
		t.Fatalf("observe after clearing: %v", err)
	}
	if cleared.ActiveRoutes != 0 {
		t.Fatalf("active_routes = %d after clearing, want 0", cleared.ActiveRoutes)
	}
}
