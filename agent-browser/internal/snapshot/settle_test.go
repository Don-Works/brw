package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// newHeadlessSettleCtx spins up a headless Chrome with the same timer-throttling
// flags production uses (see internal/cdp/launcher.go). Disabling background timer
// throttling is load-bearing for the settle quiesce path: without it, headless
// Chrome clamps setTimeout to ~1Hz, which would inflate the ~40ms quiet window to
// ~1s and make the fast-path assertion flaky. Skips when no browser is available.
func newHeadlessSettleCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		ctxCancel()
		allocCancel()
		t.Skipf("headless chrome unavailable: %v", err)
	}
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	return ctx, cancel
}

// quiesceFixture mutates the DOM once shortly after load, then goes quiet — the
// canonical "action caused a small reaction then the page settled" case. Settle
// should resolve via the mutation-quiesce path well before the cap.
const quiesceFixture = `data:text/html,` +
	`<html><body><div id="x">start</div>` +
	`<script>` +
	`setTimeout(function(){document.getElementById('x').textContent='mutated';},5);` +
	`</script>` +
	`</body></html>`

// staticFixture never mutates, navigates, or issues a network request after load.
// There is no signal that the page has "reacted", so Settle cannot resolve early
// and must degrade to the cap — proving the hard cap bounds the worst case.
const staticFixture = `data:text/html,` +
	`<html><body><div>static</div></body></html>`

// TestSettleResolvesFastOnQuiesce proves the fast path: a page that reacts then
// goes quiet settles well under the old fixed 150ms post-action delay, returning
// via the mutation-quiesce path. This is the latency win — the action no longer
// pays the full fixed sleep when the page has demonstrably settled.
func TestSettleResolvesFastOnQuiesce(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(quiesceFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	const capMS = 150
	wallStart := time.Now()
	res, err := Settle(runCtx, capMS)
	wall := time.Since(wallStart)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	// (b) Fast path: must return well under the old fixed 150ms delay. The DOM
	// mutation lands at ~5ms and the quiesce window is ~40ms, so the in-page
	// settle should report well under 100ms.
	if res.SettledMS >= 100 {
		t.Fatalf("expected fast settle well under the 150ms cap, got settledMs=%d (reason=%q)", res.SettledMS, res.Reason)
	}
	// The wall-clock CDP round-trip must also beat the old fixed sleep, proving the
	// caller actually saves time (not just the in-page clock).
	if wall >= capMS*time.Millisecond {
		t.Fatalf("wall-clock settle %v did not beat the old fixed %dms sleep", wall, capMS)
	}
	// Resolved because a network response landed (the data: navigation can surface
	// as a resource entry) OR the DOM quiesced — both are legitimate early signals.
	// It must NOT be the plain cap timeout, which would mean no early signal fired.
	if res.Reason == "cap" {
		t.Fatalf("expected an early settle signal (quiesce/network/navigation), got cap timeout; settledMs=%d", res.SettledMS)
	}
	if res.Cap != capMS {
		t.Fatalf("expected reported cap %d, got %d", capMS, res.Cap)
	}
}

// TestSettleCapsWorstCase proves the safety contract: a page with no settle signal
// resolves at the cap (reason "cap") and never overshoots it — so the worst case is
// exactly today's fixed delay, never slower.
func TestSettleCapsWorstCase(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(staticFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	// Let load fully complete so the navigation's own resource entry does not race
	// the settle and resolve it as "network".
	if err := chromedp.Run(runCtx, chromedp.WaitReady("body")); err != nil {
		t.Fatalf("wait ready: %v", err)
	}

	const capMS = 120
	res, err := Settle(runCtx, capMS)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Reason != "cap" {
		t.Fatalf("expected cap timeout on a static page, got reason=%q settledMs=%d", res.Reason, res.SettledMS)
	}
	// Hard cap: must not meaningfully overshoot the cap. Allow a small scheduling
	// slack for the in-page performance.now() read on the cap timer.
	if res.SettledMS > capMS+50 {
		t.Fatalf("settle overshot the %dms cap: settledMs=%d", capMS, res.SettledMS)
	}
}

// TestSettleZeroCapReturnsImmediately proves a zero cap is a no-op fast return, so
// call sites that pass no delay never block.
func TestSettleZeroCapReturnsImmediately(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(staticFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	res, err := Settle(runCtx, 0)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Reason != "cap" || res.SettledMS > 25 {
		t.Fatalf("expected immediate cap return, got reason=%q settledMs=%d", res.Reason, res.SettledMS)
	}
}
