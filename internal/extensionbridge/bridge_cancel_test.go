package extensionbridge

import (
	"context"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

func TestBridgeCancelSignalsRegisteredOperation(t *testing.T) {
	b := New("", 0, "")
	entry, release := b.cancels.register(context.Background(), "tab-1")
	defer release()

	res, err := b.Cancel(browser.WithTabID(context.Background(), "tab-1"), "")
	if err != nil {
		t.Fatalf("Cancel error: %v", err)
	}
	if !res.OK || res.Cancelled != 1 || res.Token != "tab-1" {
		t.Fatalf("unexpected cancel result: %+v", res)
	}
	if !entry.Cancelled() {
		t.Fatal("registered op should be cancelled")
	}
}

func TestBridgeCancelBareResolvesWildcard(t *testing.T) {
	b := New("", 0, "")
	_, release := b.cancels.register(context.Background(), "tab-a")
	defer release()

	res, _ := b.Cancel(context.Background(), "")
	if res.Token != cancelAllToken {
		t.Fatalf("bare cancel should resolve to wildcard, got %q", res.Token)
	}
	if res.Cancelled != 1 {
		t.Fatalf("wildcard cancel should signal 1, got %d", res.Cancelled)
	}
}

// TestBridgeCancelTokenAlignsWithTabContext verifies the bridge derives its
// cancel token from the same tab-id context key the browser package uses, so a
// browser_cancel with a tab_id reaches a plan/batch targeting that tab.
func TestBridgeCancelTokenAlignsWithTabContext(t *testing.T) {
	ctx := browser.WithTabID(context.Background(), "tab-99")
	if got := cancelToken(ctx, ""); got != "tab-99" {
		t.Fatalf("bridge cancel token = %q, want tab-99", got)
	}
	if got := cancelToken(context.Background(), "explicit"); got != "explicit" {
		t.Fatalf("explicit token should win, got %q", got)
	}
}
