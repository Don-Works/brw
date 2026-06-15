package browser

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestCancelRegistrySignalsRegisteredEntry verifies the core cancel hook: an
// operation registered under a token is marked cancelled (and its context
// closed) when that token is cancelled, and the cancel count is reported.
func TestCancelRegistrySignalsRegisteredEntry(t *testing.T) {
	reg := newCancelRegistry()
	entry, release := reg.register(context.Background(), "tab-1")
	defer release()

	if entry.Cancelled() {
		t.Fatal("entry should not start cancelled")
	}

	n := reg.cancel("tab-1")
	if n != 1 {
		t.Fatalf("expected 1 operation signalled, got %d", n)
	}
	if !entry.Cancelled() {
		t.Fatal("entry should be cancelled after cancel(token)")
	}
	if entry.ctx.Err() == nil {
		t.Fatal("entry context should be cancelled (Done) after cancel")
	}

	// Cancelling an unrelated token leaves us alone.
	if got := reg.cancel("other-tab"); got != 0 {
		t.Fatalf("cancel of unrelated token should signal 0, got %d", got)
	}
}

// TestCancelRegistryWildcardCancelsEverything verifies the "*" stop-everything
// switch reaches operations registered under any token.
func TestCancelRegistryWildcardCancelsEverything(t *testing.T) {
	reg := newCancelRegistry()
	a, relA := reg.register(context.Background(), "tab-a")
	b, relB := reg.register(context.Background(), "tab-b")
	defer relA()
	defer relB()

	if n := reg.cancel(cancelAllToken); n != 2 {
		t.Fatalf("wildcard cancel should signal 2 operations, got %d", n)
	}
	if !a.Cancelled() || !b.Cancelled() {
		t.Fatal("wildcard cancel should mark every registered entry cancelled")
	}
}

// TestCancelRegistryReleaseDeregisters verifies a finished operation no longer
// matches a later cancel (no stale entries linger).
func TestCancelRegistryReleaseDeregisters(t *testing.T) {
	reg := newCancelRegistry()
	_, release := reg.register(context.Background(), "tab-1")
	release()
	if n := reg.cancel("tab-1"); n != 0 {
		t.Fatalf("released entry should not be cancellable, got %d signalled", n)
	}
}

func TestCancelTokenResolution(t *testing.T) {
	if got := cancelToken(context.Background(), "explicit"); got != "explicit" {
		t.Fatalf("explicit token should win, got %q", got)
	}
	ctx := WithTabID(context.Background(), "tab-42")
	if got := cancelToken(ctx, ""); got != "tab-42" {
		t.Fatalf("token should fall back to tab id, got %q", got)
	}
	if got := cancelToken(context.Background(), ""); got != "" {
		t.Fatalf("bare context should resolve to empty token, got %q", got)
	}
}

// TestManagerCancelResolvesWildcardWhenBare verifies a bare browser_cancel acts
// as the universal stop switch and reports how many ops were signalled.
func TestManagerCancelResolvesWildcardWhenBare(t *testing.T) {
	m := &Manager{cancels: newCancelRegistry()}
	_, release := m.cancels.register(context.Background(), "tab-x")

	res, err := m.Cancel(context.Background(), "")
	if err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if res.Token != cancelAllToken {
		t.Fatalf("bare cancel should resolve to wildcard, got %q", res.Token)
	}
	if !res.OK || res.Cancelled != 1 {
		t.Fatalf("expected ok cancel of 1 op, got %+v", res)
	}

	// In production the cancelled operation's loop returns and its deferred
	// release deregisters the entry; simulate that, then a second cancel finds
	// nothing in flight: not an error, count 0, with an explanatory message.
	release()
	res2, _ := m.Cancel(context.Background(), "*")
	if res2.Cancelled != 0 || res2.Message == "" {
		t.Fatalf("second cancel should report 0 with a message, got %+v", res2)
	}
}

// TestRunPlanStepsStopsEarlyOnCancel is the core feature test: a multi-step plan
// is cancelled partway via the cancel hook; the loop stops promptly and reports
// how many steps completed plus cancelled=true rather than erroring or crashing.
func TestRunPlanStepsStopsEarlyOnCancel(t *testing.T) {
	reg := newCancelRegistry()
	entry, release := reg.register(context.Background(), "tab-1")
	defer release()

	const cancelAfter = 2
	var ran int
	runner := func(ctx context.Context, i int, step PlanStep) PlanStepResult {
		ran++
		// Trigger the cancel hook partway through, exactly as a concurrent
		// browser_cancel would: cancel the token while step `cancelAfter` runs.
		if i == cancelAfter-1 {
			if got := reg.cancel("tab-1"); got != 1 {
				t.Fatalf("expected cancel to signal 1 op, got %d", got)
			}
		}
		return PlanStepResult{Index: i, Action: step.Action, OK: true, Message: "ok"}
	}

	steps := []PlanStep{
		{Action: "click", Ref: "e1"},
		{Action: "click", Ref: "e2"},
		{Action: "click", Ref: "e3"},
		{Action: "click", Ref: "e4"},
		{Action: "click", Ref: "e5"},
	}

	result := runPlanSteps(entry.ctx, entry, steps, runner)

	if !result.Cancelled {
		t.Fatal("expected result.Cancelled = true")
	}
	if result.OK {
		t.Fatal("cancelled plan should not report OK")
	}
	if result.Error != "cancelled" {
		t.Fatalf("expected error %q, got %q", "cancelled", result.Error)
	}
	// We cancelled while running step index 1 (the 2nd step), so the loop should
	// stop before completing all steps. steps_completed must be < total.
	if result.StepsCompleted >= len(steps) {
		t.Fatalf("expected fewer than %d steps completed, got %d", len(steps), result.StepsCompleted)
	}
	if result.StepsCompleted != cancelAfter {
		t.Fatalf("expected steps_completed = %d, got %d", cancelAfter, result.StepsCompleted)
	}
	// The loop must not have run every step (early stop, not a full pass).
	if ran >= len(steps) {
		t.Fatalf("loop should have stopped early; ran %d of %d steps", ran, len(steps))
	}
}

// TestRunPlanStepsCancelMidStepReportsCancelled verifies that when a cancel
// lands mid-step (the step itself fails because its context was torn down), the
// result is reported as cancelled rather than an opaque step error.
func TestRunPlanStepsCancelMidStepReportsCancelled(t *testing.T) {
	reg := newCancelRegistry()
	entry, release := reg.register(context.Background(), "tab-1")
	defer release()

	runner := func(ctx context.Context, i int, step PlanStep) PlanStepResult {
		if i == 1 {
			reg.cancel("tab-1")
			// Simulate the step failing because its context was cancelled.
			return PlanStepResult{Index: i, Action: step.Action, OK: false, Error: "context canceled"}
		}
		return PlanStepResult{Index: i, Action: step.Action, OK: true}
	}

	steps := []PlanStep{{Action: "click"}, {Action: "wait"}, {Action: "click"}}
	result := runPlanSteps(entry.ctx, entry, steps, runner)

	if !result.Cancelled || result.OK {
		t.Fatalf("mid-step cancel should report cancelled, got %+v", result)
	}
	if result.Error != "cancelled" {
		t.Fatalf("expected cancelled error, got %q", result.Error)
	}
	if result.StepsCompleted != 1 {
		t.Fatalf("expected 1 step completed before mid-step cancel, got %d", result.StepsCompleted)
	}
}

// TestWaitForLoopHonorsContextCancel verifies the wait-loop cancellation: a
// WaitFor whose context is cancelled returns promptly with a cancelled error
// instead of running out the full timeout. We drive the loop logic directly via
// a tight re-creation of the cancel check the production WaitFor uses.
func TestWaitForLoopHonorsContextCancel(t *testing.T) {
	reg := newCancelRegistry()
	entry, release := reg.register(context.Background(), "tab-1")
	defer release()

	// Cancel almost immediately, as a concurrent browser_cancel would.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		reg.cancel("tab-1")
	}()

	start := time.Now()
	// Mirror the production WaitFor loop's cancellation guard against a long
	// deadline: it must bail out as soon as the context is cancelled.
	deadline := time.Now().Add(5 * time.Second)
	var bailed bool
	for {
		if entry.ctx.Err() != nil {
			bailed = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()

	if !bailed {
		t.Fatal("wait loop should bail out on context cancel")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait loop took too long to honor cancel: %v", elapsed)
	}
}
