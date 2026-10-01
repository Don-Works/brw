package extensionbridge

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestBridgeWaitDeadlineMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_BRIDGE_WAIT_DEADLINE") != "1" {
		t.Skip("set BRW_MEASURE_BRIDGE_WAIT_DEADLINE=1 for disposable background-tab measurements")
	}
	for _, normalTimers := range []bool{true, false} {
		t.Run(fmt.Sprintf("native_timers_%t", normalTimers), func(t *testing.T) {
			b, ctx, _ := deadlineLiveBridge(t, normalTimers)
			for round := 0; round < 12; round++ {
				started := time.Now()
				outcome, err := b.WaitForOutcome(ctx, "fn:new Promise(()=>{})", 40*time.Millisecond)
				waitMS := float64(time.Since(started).Microseconds()) / 1000
				if err == nil || outcome.OK {
					t.Fatal("never-resolving predicate succeeded")
				}
				started = time.Now()
				result, batchErr := b.ExecuteBatch(ctx, []browser.BatchStep{{Action: "wait", Condition: "fn:new Promise(()=>{})", TimeoutMS: 40}})
				batchMS := float64(time.Since(started).Microseconds()) / 1000
				if batchErr != nil || result.OK || result.StepsCompleted != 1 || !strings.Contains(result.Error, "timed out") {
					t.Fatalf("batch failed incorrectly: %+v err=%v", result, batchErr)
				}
				started = time.Now()
				snap, snapErr := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
				snapshotMS := float64(time.Since(started).Microseconds()) / 1000
				if snapErr != nil || len(snap.Elements) != 3 {
					t.Fatalf("post-wait snapshot=%+v err=%v", snap, snapErr)
				}
				row, _ := json.Marshal(map[string]any{"native_timers": normalTimers, "round": round, "wait_ms": waitMS, "batch_ms": batchMS, "snapshot_ms": snapshotMS, "wakeups": outcome.Wakeups, "result_ok": result.OK, "steps_completed": result.StepsCompleted})
				t.Log(string(row))
			}
		})
	}
}

func TestBridgeWaitLiveStrictDeadlineAndCleanup(t *testing.T) {
	if os.Getenv("BRW_MEASURE_BRIDGE_WAIT_DEADLINE") != "1" {
		t.Skip("set BRW_MEASURE_BRIDGE_WAIT_DEADLINE=1 for disposable delayed-renderer correctness")
	}
	b, ctx, _ := deadlineLiveBridge(t, false)
	_, err := b.Evaluate(ctx, `window.fixtureSetTimeout=setTimeout;window.setTimeout=(fn,ms,...args)=>window.fixtureSetTimeout(fn,Math.max(1000,ms),...args);window.fixtureObservers=0;window.MutationObserver=class extends MutationObserver{constructor(fn){super(fn);window.fixtureObservers++}disconnect(){super.disconnect();window.fixtureObservers--}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, condition := range []string{"fn:new Promise(()=>{})", "fn:false", "fn:new Promise(resolve=>window.fixtureSetTimeout(()=>resolve(true),150))"} {
		started := time.Now()
		outcome, err := b.WaitForOutcome(ctx, condition, 40*time.Millisecond)
		elapsed := time.Since(started)
		if err == nil || outcome.OK || !strings.Contains(err.Error(), "timed out waiting") || elapsed < 40*time.Millisecond || elapsed > 200*time.Millisecond {
			t.Fatalf("delayed renderer condition=%s outcome=%+v err=%v elapsed=%s", condition, outcome, err, elapsed)
		}
		cleanupDeadline := time.Now().Add(time.Second)
		for {
			state, err := b.Evaluate(ctx, `({keys:Object.keys(window).filter(k=>k.startsWith('__brw_wait_')).length,observers:window.fixtureObservers})`)
			if err != nil {
				t.Fatal(err)
			}
			values := state.(map[string]any)
			if values["keys"] == float64(0) && values["observers"] == float64(0) {
				break
			}
			if time.Now().After(cleanupDeadline) {
				t.Fatalf("wait cleanup leaked: %v", state)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("strict deadline condition=%s waited_ms=%.3f registry/observers=0", condition, float64(elapsed.Microseconds())/1000)
	}
	for _, condition := range []string{"fn:true", "fn:Promise.resolve(true)"} {
		started := time.Now()
		outcome, err := b.WaitForOutcome(ctx, condition, 40*time.Millisecond)
		if err != nil || !outcome.OK || time.Since(started) > 100*time.Millisecond {
			t.Fatalf("same-tick success lost: %s outcome=%+v err=%v", condition, outcome, err)
		}
	}
	started := time.Now()
	result, err := b.ExecuteBatch(ctx, []browser.BatchStep{{Action: "wait", Condition: "fn:new Promise(()=>{})", TimeoutMS: 40}, {Action: "fill", Ref: "fixture-unreachable", Text: "wrong"}})
	if err != nil || result.OK || result.StepsCompleted != 1 || len(result.Steps) != 1 || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("failed wait did not bound/stop batch: %+v err=%v elapsed=%s", result, err, time.Since(started))
	}
}
