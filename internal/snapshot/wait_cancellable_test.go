package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestCancellableWaitAsyncPredicateDoesNotRearmAfterCancellation(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank"), chromedp.Evaluate(`window.activeObservers=0;window.MutationObserver=class extends MutationObserver{constructor(fn){super(fn);window.activeObservers++}disconnect(){super.disconnect();window.activeObservers--}}`, nil)); err != nil {
		t.Fatal(err)
	}
	owner, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		time.Sleep(40 * time.Millisecond)
		stop()
	}()
	matched, err := WaitForConditionCancellable(owner, `fn:new Promise(resolve=>setTimeout(()=>resolve(false),120))`, 2000)
	if err == nil || matched {
		t.Fatalf("cancelled predicate returned success: matched=%t err=%v", matched, err)
	}
	var state struct {
		Keys      int `json:"keys"`
		Observers int `json:"observers"`
	}
	expr := `new Promise(resolve=>setTimeout(()=>resolve({keys:Object.keys(window).filter(k=>k.startsWith('__brw_wait_')).length,observers:window.activeObservers}),160))`
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &state, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	if state.Keys != 0 || state.Observers != 0 {
		t.Fatalf("cancelled async predicate rearmed a watcher: %+v", state)
	}
}

func TestWaitAsyncPredicateHonorsDeadline(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	for _, wait := range []func(context.Context, string, int64) (bool, error){WaitForCondition, WaitForConditionCancellable} {
		for _, condition := range []string{
			"fn:new Promise(()=>{})",
			"fn:new Promise(resolve=>setTimeout(()=>resolve(true),150))",
			"fn:new Promise(resolve=>setTimeout(()=>resolve(false),150))",
		} {
			bounded, stop := context.WithTimeout(ctx, 400*time.Millisecond)
			started := time.Now()
			matched, err := wait(bounded, condition, 40)
			elapsed := time.Since(started)
			stop()
			if err != nil || matched || elapsed > 250*time.Millisecond {
				t.Fatalf("condition=%s matched=%t err=%v elapsed=%s", condition, matched, err, elapsed)
			}
		}
	}
}
