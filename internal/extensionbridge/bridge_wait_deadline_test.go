package extensionbridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestBridgeWaitStrictDeadlineWhenRendererDoesNotAnswer(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/x", "https://final.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.hangWaitScript = true
	fake.mu.Unlock()
	ctx := browser.WithTabID(context.Background(), "42")
	started := time.Now()
	outcome, err := b.WaitForOutcome(ctx, "fn:new Promise(()=>{})", 40*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil || outcome.OK || !strings.Contains(err.Error(), "timed out waiting") || elapsed < 40*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("wait outcome=%+v err=%v elapsed=%s", outcome, err, elapsed)
	}
}

func TestBridgeWaitRejectsRepliesPastDeadline(t *testing.T) {
	b := New("", 5*time.Second, "")
	stub := serveRPCStub(t, b, func(msgType string, call int) (map[string]any, bool, string) {
		time.Sleep(120 * time.Millisecond)
		return map[string]any{"result": map[string]any{"value": true}}, true, ""
	})
	defer stub.stop()
	ctx := browser.WithTabID(context.Background(), "42")
	started := time.Now()
	outcome, err := b.WaitForOutcome(ctx, "fn:false", 40*time.Millisecond)
	if err == nil || outcome.OK || !strings.Contains(err.Error(), "timed out waiting") || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("late success accepted: outcome=%+v err=%v elapsed=%s", outcome, err, time.Since(started))
	}
}

func TestBridgeWaitMidAwaitCancellationRemainsCancellation(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/x", "https://final.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.hangWaitScript = true
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(browser.WithTabID(context.Background(), "42"))
	defer cancel()
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	started := time.Now()
	outcome, err := b.WaitForOutcome(ctx, "fn:new Promise(()=>{})", time.Second)
	if err == nil || outcome.OK || !strings.Contains(err.Error(), "cancelled") || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("cancelled wait outcome=%+v err=%v elapsed=%s", outcome, err, time.Since(started))
	}
}
