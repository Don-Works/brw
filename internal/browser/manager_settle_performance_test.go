package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// These two tests each launch Chrome and then take ten timed samples under one
// tab context. Both budgets are deliberately far above what the work costs: they
// exist to stop an unrelated deadline ending a run, not to bound the timings,
// which the tests assert on directly.
const (
	settlePerformanceBudget = 120 * time.Second
	settleMeasurementBudget = 90 * time.Second
)

// TestPrearmedSettleIsMateriallyFaster is a regression/benchmark gate for the
// observer-ordering bug: a settle observer installed after a synchronous DOM
// change cannot see it and burns the full cap. Measure the settle mechanism
// directly so unrelated actionability checks and post-action snapshots cannot
// turn host load into timing noise. Production actions use this same helper and
// should finish after the 40 ms quiescence window.
func TestPrearmedSettleIsMateriallyFaster(t *testing.T) {
	chromePath, err := cdplaunch.FindChrome("")
	if err != nil {
		t.Skipf("Chrome unavailable: %v", err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><button id="trigger">Update</button><output id="result">0</output><script>let n=0;trigger.onclick=()=>{result.textContent=String(++n)}</script>`)
	}))
	defer fixture.Close()

	// Sized for a Chrome launch plus the whole measurement loop on a loaded
	// machine, not for one action: a sample that dies on a deadline reports a
	// context error instead of the timing the test exists to record.
	ctx, cancel := context.WithTimeout(context.Background(), settlePerformanceBudget)
	defer cancel()
	profile := browsertest.NewProfile(t)
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: profile.Dir(), Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	profile.StopWith(func() { _ = manager.Close() })
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatal(err)
	}
	// Every sample shares this context, so it carries the loop's budget rather
	// than Manager.timeout, which is the allowance for a single round trip.
	_, tabCtx, release, err := manager.activeContextWithTimeout(ctx, settleMeasurementBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	action := func() error {
		return chromedp.Run(tabCtx, chromedp.Evaluate(`document.getElementById("trigger").click()`, nil))
	}

	const (
		samples = 5
		// Keep the measured signal well above host-scheduler/CDP jitter. This does
		// not change production's 150 ms cap; both paths below receive the same cap,
		// and the separate snapshot tests exercise the production-sized boundary.
		benchmarkCap = 500 * time.Millisecond
	)
	production := make([]time.Duration, 0, samples)
	legacy := make([]time.Duration, 0, samples)
	for range samples {
		started := time.Now()
		if err := manager.runWithPrearmedSettle(tabCtx, benchmarkCap, action); err != nil {
			t.Fatal(err)
		}
		production = append(production, time.Since(started))

		started = time.Now()
		if err := action(); err != nil {
			t.Fatal(err)
		}
		_, _ = snapshot.Settle(tabCtx, benchmarkCap.Milliseconds())
		legacy = append(legacy, time.Since(started))
	}
	sort.Slice(production, func(i, j int) bool { return production[i] < production[j] })
	sort.Slice(legacy, func(i, j int) bool { return legacy[i] < legacy[j] })
	newMedian, oldMedian := production[samples/2], legacy[samples/2]
	if newMedian*4 >= oldMedian*3 {
		t.Fatalf("prearmed median=%s legacy-postarmed median=%s; want at least 25%% faster", newMedian, oldMedian)
	}
	t.Logf("prearmed median=%s, legacy post-armed median=%s (%.2fx faster)", newMedian, oldMedian, float64(oldMedian)/float64(newMedian))
}

// TestPrearmedSettleWorstCaseOverheadIsBounded measures the honest tradeoff:
// when an action causes no observable reaction, both designs wait for the cap
// and pre-arming costs one small CDP round trip. That overhead must stay bounded
// rather than erasing the fast-path win or turning a cap into an unbounded wait.
func TestPrearmedSettleWorstCaseOverheadIsBounded(t *testing.T) {
	chromePath, err := cdplaunch.FindChrome("")
	if err != nil {
		t.Skipf("Chrome unavailable: %v", err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><button id="noop">No operation</button>`)
	}))
	defer fixture.Close()

	// Sized for a Chrome launch plus the whole measurement loop on a loaded
	// machine, not for one action: a sample that dies on a deadline reports a
	// context error instead of the timing the test exists to record.
	ctx, cancel := context.WithTimeout(context.Background(), settlePerformanceBudget)
	defer cancel()
	profile := browsertest.NewProfile(t)
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: profile.Dir(), Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	profile.StopWith(func() { _ = manager.Close() })
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatal(err)
	}
	// Every sample shares this context, so it carries the loop's budget rather
	// than Manager.timeout, which is the allowance for a single round trip.
	_, tabCtx, release, err := manager.activeContextWithTimeout(ctx, settleMeasurementBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	const samples = 5
	const cap = 100 * time.Millisecond
	action := func() error {
		return chromedp.Run(tabCtx, chromedp.Evaluate(`document.getElementById("noop").click()`, nil))
	}
	round := func() (time.Duration, time.Duration) {
		prearmed := make([]time.Duration, 0, samples)
		legacy := make([]time.Duration, 0, samples)
		for range samples {
			started := time.Now()
			if err := manager.runWithPrearmedSettle(tabCtx, cap, action); err != nil {
				t.Fatal(err)
			}
			prearmed = append(prearmed, time.Since(started))

			started = time.Now()
			if err := action(); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshot.Settle(tabCtx, cap.Milliseconds()); err != nil {
				t.Fatal(err)
			}
			legacy = append(legacy, time.Since(started))
		}
		sort.Slice(prearmed, func(i, j int) bool { return prearmed[i] < prearmed[j] })
		sort.Slice(legacy, func(i, j int) bool { return legacy[i] < legacy[j] })
		return prearmed[samples/2], legacy[samples/2]
	}

	// This is a wall-clock comparison taken while the rest of the suite is also
	// launching browsers, so one round is noisy. Every round is measured and the
	// MIDDLE overhead has to be within budget.
	//
	// Stopping at the first round under budget would be a weaker gate, not a
	// steadier one. Contention inflates BOTH paths, and it is the legacy path
	// being inflated that makes the difference look small — so three chances to
	// catch the legacy path on a slow round is three chances for a real overhead
	// to be masked. The median needs two rounds out of three to agree, which one
	// unlucky round cannot buy and a structural regression never gets.
	const rounds = 3
	overheads := make([]time.Duration, 0, rounds)
	for attempt := 1; attempt <= rounds; attempt++ {
		newMedian, oldMedian := round()
		overheads = append(overheads, newMedian-oldMedian)
		t.Logf("round %d: prearmed=%s legacy=%s (overhead %s)", attempt, newMedian, oldMedian, newMedian-oldMedian)
	}
	sorted := append([]time.Duration(nil), overheads...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if median := sorted[rounds/2]; median > 50*time.Millisecond {
		t.Fatalf("prearmed no-reaction overhead median=%s over %d rounds (%v); overhead exceeds 50ms", median, rounds, overheads)
	}
	t.Logf("no-reaction overhead median=%s over %d rounds (%v)", sorted[rounds/2], rounds, overheads)
}
