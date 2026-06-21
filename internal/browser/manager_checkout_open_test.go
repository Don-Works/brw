package browser

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/target"
)

// TestCheckoutClickProceeds is the end-to-end proof that asking the agent to
// click a place-order button actuates it — there is no purchase/checkout gate.
// It drives the same Manager.Click path the MCP brw_click tool uses.
func TestCheckoutClickProceeds(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	const html = `<button id='b' style='position:absolute;left:20px;top:20px;width:220px;height:60px'>Place order</button>` +
		`<script>window.__placed=0;document.getElementById('b').addEventListener('click',function(){window.__placed++;});</script>`

	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html," + url.PathEscape(html)).Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))

	// Snapshot assigns a stable ref to the place-order button.
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Query: "Place order", Limit: 5})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Elements) == 0 {
		t.Fatal("place-order button not found in snapshot")
	}
	ref := snap.Elements[0].Ref

	// Click it through the production path. Under the open default this must
	// succeed and fire the handler — no policy block.
	if _, err := m.Click(ctx, ref); err != nil {
		t.Fatalf("checkout click should proceed under the open default, got %v", err)
	}
	placed, err := m.Evaluate(ctx, `Number(window.__placed||0)`)
	if err != nil {
		t.Fatalf("eval placed: %v", err)
	}
	if n, _ := placed.(float64); n < 1 {
		t.Fatalf("place-order click did not actuate (handler fired %v times)", placed)
	}
}
