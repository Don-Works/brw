package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestNotifyChecksDestinationBeforeEffectsAndBeforeReturning(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	_, tabCtx, cancelTab, err := m.activeContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTab()
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`window.fixtureNotificationAccesses=0; Object.defineProperty(window,'Notification',{configurable:true,get(){window.fixtureNotificationAccesses++;return undefined;}})`, nil)); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("owned notification destination refused")
	for _, tc := range []struct {
		name                 string
		denyAt, wantAccesses int
	}{
		{"preflight", 1, 0},
		{"postflight", 2, 1},
		{"allowed_unavailable", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := chromedp.Run(tabCtx, chromedp.Evaluate(`window.fixtureNotificationAccesses=0`, nil)); err != nil {
				t.Fatal(err)
			}
			checks := 0
			guarded := WithFrameReadCheck(ctx, func(raw string) error {
				if raw != "about:blank" {
					t.Fatalf("checked unrelated destination %q", raw)
				}
				checks++
				if checks == tc.denyAt {
					return denied
				}
				return nil
			})
			out, err := m.Notify(guarded, NotifyOptions{Kind: "done", Message: "Owned fixture only"})
			if tc.denyAt != 0 {
				if !errors.Is(err, denied) || out != (NotifyResult{}) {
					t.Fatalf("refused notification retained output: %+v err=%v", out, err)
				}
			} else if err != nil || out.OK || out.Delivery != "unavailable" || checks != 2 {
				t.Fatalf("allowed unavailable notification: %+v checks=%d err=%v", out, checks, err)
			}
			var accesses int
			if err := chromedp.Run(tabCtx, chromedp.Evaluate(`window.fixtureNotificationAccesses`, &accesses)); err != nil {
				t.Fatal(err)
			}
			if accesses != tc.wantAccesses {
				t.Fatalf("page notification accesses=%d want=%d", accesses, tc.wantAccesses)
			}
		})
	}
}
