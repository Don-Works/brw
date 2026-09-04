package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// TestPrearmedSettleIsMateriallyFaster is a regression/benchmark gate for the
// observer-ordering bug: a settle observer installed after a synchronous DOM
// change cannot see it and burns the full cap. Production actions arm first and
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
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: t.TempDir(), Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatal(err)
	}
	snap, err := manager.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ref := ""
	for _, element := range snap.Elements {
		if element.Role == "button" && element.Name == "Update" {
			ref = element.Ref
			break
		}
	}
	if ref == "" {
		t.Fatal("fixture button not found")
	}

	const samples = 5
	production := make([]time.Duration, 0, samples)
	legacy := make([]time.Duration, 0, samples)
	for range samples {
		started := time.Now()
		if _, err := manager.Click(ctx, ref); err != nil {
			t.Fatal(err)
		}
		production = append(production, time.Since(started))

		_, tabCtx, release, err := manager.activeContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		started = time.Now()
		if err := chromedp.Run(tabCtx, chromedp.Evaluate(`document.getElementById("trigger").click()`, nil)); err != nil {
			release()
			t.Fatal(err)
		}
		_, _ = snapshot.Settle(tabCtx, actionSettleDelay.Milliseconds())
		legacy = append(legacy, time.Since(started))
		release()
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
	manager, err := New(ctx, Config{
		ChromePath: chromePath, UserDataDir: t.TempDir(), Timeout: 10 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
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
