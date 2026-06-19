package browser

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/target"
)

// TestSnapshotAccessibilityRequestedFlag proves SPEC 1: the Snapshot accessibility
// summary distinguishes "not requested" from "requested but failed".
//   - IncludeAX=false (the default): Requested=false, Available=false — an honest
//     "you didn't ask for it" signal, not a false "AX unavailable".
//   - IncludeAX=true with a normal page: Requested=true, Available=true, NodeCount>0.
func TestSnapshotAccessibilityRequestedFlag(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	html := `<!DOCTYPE html><html><body><h1>AX Page</h1><button>Press</button><a href="/x">Link</a></body></html>`
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html," + url.PathEscape(html)).Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))

	// Default: accessibility not requested.
	noAX, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot (no ax): %v", err)
	}
	if noAX.Accessibility.Requested {
		t.Fatalf("Requested=true when IncludeAX=false; want false (summary: %+v)", noAX.Accessibility)
	}
	if noAX.Accessibility.Available {
		t.Fatalf("Available=true when IncludeAX=false; want false (summary: %+v)", noAX.Accessibility)
	}

	// Requested + success: all three positive signals present.
	withAX, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeAX: true})
	if err != nil {
		t.Fatalf("snapshot (ax): %v", err)
	}
	if !withAX.Accessibility.Requested {
		t.Fatalf("Requested=false when IncludeAX=true; want true (summary: %+v)", withAX.Accessibility)
	}
	if !withAX.Accessibility.Available {
		t.Fatalf("Available=false when IncludeAX=true on a normal page; want true (summary: %+v)", withAX.Accessibility)
	}
	if withAX.Accessibility.NodeCount == 0 {
		t.Fatalf("NodeCount=0 when AX succeeded; want >0 (summary: %+v)", withAX.Accessibility)
	}
}
