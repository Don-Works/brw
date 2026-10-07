package testbed

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/harness"
)

func TestBrowserFixtureReconnectsAndReadsGroundTruth(t *testing.T) {
	if os.Getenv("BRW_TESTBED_LIVE") != "1" {
		t.Skip("set BRW_TESTBED_LIVE=1 for one disposable headless browser")
	}
	s := start(t, 7, 24)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rig, err := harness.LaunchBrowser(ctx, harness.BrowserOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	opened, err := rig.Manager.Open(ctx, s.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Manager.CloseTab(ctx, opened.Tab.ID)
	ctx = browser.WithTabID(ctx, opened.Tab.ID)
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("fixture page did not reach its expected state")
	}
	wait(func() bool {
		oracle := state(t, s)
		return oracle.SSEConnectionCount == 1 && oracle.WSConnectionCount == 1
	})
	read, err := rig.Manager.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"Ada North", "DELTA-0007", "127 litres", "The corrected reading supersedes the sidebar estimate."} {
		if !strings.Contains(read.Main, phrase) {
			t.Fatalf("reading omitted ground truth %q", phrase)
		}
	}
	initial := state(t, s)
	step(t, s, initial.RunID, "hydrate", 1)
	step(t, s, initial.RunID, "disconnect", 1)
	wait(func() bool {
		oracle := state(t, s)
		return oracle.AppliedCursor == 2 && oracle.AcknowledgedCursor == 2 && oracle.SSEConnectionCount >= 2 && oracle.WSConnectionCount >= 2
	})
	step(t, s, initial.RunID, "virtualize", 1)
	wait(func() bool { oracle := state(t, s); return oracle.AppliedCursor == 3 && oracle.AcknowledgedCursor == 3 })
	value, err := rig.Manager.Evaluate(ctx, `JSON.stringify({epoch:document.querySelector('[data-testid="stable-action"]').dataset.epoch,rows:[...document.querySelectorAll('[data-item-id]')].map(e=>e.dataset.itemId),cursor:document.getElementById('cursor-status').textContent,error:document.getElementById('error').textContent})`)
	if err != nil {
		t.Fatal(err)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("DOM evidence has type %T", value)
	}
	if !strings.Contains(text, `"epoch":"2"`) || !strings.Contains(text, "Applied cursor 3") || !strings.Contains(text, `"error":""`) {
		t.Fatalf("DOM failed to apply the sequence: %s", text)
	}
	for _, id := range state(t, s).VisibleItemIDs {
		if !strings.Contains(text, id) {
			t.Fatalf("virtual row %s absent", id)
		}
	}
	if screenshotPath := os.Getenv("BRW_TESTBED_SCREENSHOT"); screenshotPath != "" {
		shot, err := rig.Manager.Screenshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(screenshotPath, shot.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("owned headless page read, hydration, SSE/WS reconnect, and exactly-once cursor application verified; reading chars=%d", len([]rune(read.Main)))
}
