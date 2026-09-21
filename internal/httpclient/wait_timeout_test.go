package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A wait's own timeout_ms has to be able to exceed the client's flat timeout,
// or a proxying daemon cuts off waits the upstream is still legitimately
// running.
func TestWaitForOutcomeOutlastsTheFlatClientTimeout(t *testing.T) {
	const upstreamTakes = 600 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(upstreamTakes)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"condition":"ready","resolved_by":"script","wakeups":1}`)
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.WaitForOutcome(ctx, "ready", 0); err == nil {
		t.Fatal("a wait with no timeout of its own outlasted the 200ms client timeout; the control case no longer proves anything")
	}
	out, err := c.WaitForOutcome(ctx, "ready", 2*time.Second)
	if err != nil {
		t.Fatalf("wait with timeout_ms above the client timeout: %v", err)
	}
	if !out.OK || out.ResolvedBy != "script" {
		t.Fatalf("outcome = %+v, want the upstream's answer", out)
	}
}
