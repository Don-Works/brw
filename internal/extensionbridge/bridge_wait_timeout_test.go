package extensionbridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// brw_wait_for's timeout_ms was only the in-page timer. When the renderer never
// answered the chunk's Runtime.evaluate, the round trip ran to the daemon's
// --timeout (here 4s) and surfaced as a bare "context deadline exceeded".
func TestWaitForHonoursTimeoutWhenTheRendererNeverAnswers(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/x", "https://final.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.hangWaitScript = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 10*time.Second)
	defer cancel()

	const timeout = 300 * time.Millisecond
	started := time.Now()
	outcome, err := b.WaitForOutcome(ctx, "text:never", timeout)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("wait resolved (%+v) although the page never answered", outcome)
	}
	if !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("error = %q, want the wait's own timeout message", err)
	}
	if elapsed > timeout+waitChunkGrace+time.Second {
		t.Fatalf("wait took %s for a %s timeout; the round trip ran to the daemon timeout instead", elapsed, timeout)
	}
	if elapsed < timeout {
		t.Fatalf("wait returned after %s, before its %s timeout", elapsed, timeout)
	}
}

// A bridge request the extension never answers used to fail with the bare
// context error, which reads as a timeout the caller chose. It names the
// request and how long it waited.
func TestUnansweredBridgeRequestIsNamed(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/x", "https://final.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.hangMethods = map[string]bool{"Page.hangs": true}
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 400*time.Millisecond)
	defer cancel()

	_, err := b.cdp(ctx, "42", "Page.hangs", nil)
	if err == nil {
		t.Fatal("an unanswered request succeeded")
	}
	if !strings.Contains(err.Error(), "no reply to cdp Page.hangs within") {
		t.Fatalf("error = %q, want it to name the request that went unanswered", err)
	}
	if !strings.Contains(err.Error(), "busy or hung") {
		t.Fatalf("error = %q, want it to say what an unanswered request means", err)
	}
}
