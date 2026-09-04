package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profileDir := t.TempDir()
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: profileDir, Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	cleanupPerformanceManager(t, manager, profileDir)
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatal(err)
	}
	_, tabCtx, release, err := manager.activeContext(ctx)
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
		if err := runWithPrearmedSettle(tabCtx, benchmarkCap, action); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profileDir := t.TempDir()
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: profileDir, Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	cleanupPerformanceManager(t, manager, profileDir)
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatal(err)
	}
	_, tabCtx, release, err := manager.activeContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	const samples = 5
	const cap = 100 * time.Millisecond
	prearmed := make([]time.Duration, 0, samples)
	legacy := make([]time.Duration, 0, samples)
	action := func() error {
		return chromedp.Run(tabCtx, chromedp.Evaluate(`document.getElementById("noop").click()`, nil))
	}
	for range samples {
		started := time.Now()
		if err := runWithPrearmedSettle(tabCtx, cap, action); err != nil {
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
	newMedian, oldMedian := prearmed[samples/2], legacy[samples/2]
	if newMedian > oldMedian+50*time.Millisecond {
		t.Fatalf("prearmed no-reaction median=%s legacy=%s; overhead exceeds 50ms", newMedian, oldMedian)
	}
	t.Logf("no-reaction median: prearmed=%s legacy=%s (overhead %s)", newMedian, oldMedian, newMedian-oldMedian)
}

// Chrome's root process can exit a few milliseconds before its last helper
// releases or finishes writing the profile on Linux. Manager.Close waits for
// the root process; this test-only cleanup additionally requires the disposable
// profile to remain absent for a short quiet window before testing.TempDir runs
// its strict one-shot cleanup. A helper that actually stays alive still fails
// the bounded deadline instead of being hidden as a flaky RemoveAll error.
func cleanupPerformanceManager(t *testing.T, manager *Manager, profileDir string) {
	t.Helper()
	t.Cleanup(func() {
		_ = manager.Close()
		deadline := time.Now().Add(3 * time.Second)
		var (
			lastErr      error
			missingSince time.Time
		)
		for {
			lastErr = os.RemoveAll(profileDir)
			_, statErr := os.Stat(profileDir)
			if lastErr == nil && os.IsNotExist(statErr) {
				if missingSince.IsZero() {
					missingSince = time.Now()
				}
				if time.Since(missingSince) >= 150*time.Millisecond {
					return
				}
			} else {
				missingSince = time.Time{}
				if lastErr == nil {
					lastErr = statErr
				}
			}
			if time.Now().After(deadline) {
				t.Errorf("temporary Chrome profile did not quiesce after Manager.Close: %v", lastErr)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
}
