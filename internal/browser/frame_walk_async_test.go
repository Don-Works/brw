package browser

import (
	"context"
	"testing"
	"time"
)

// TestAssertFindsRefInLateAttachedShadowRoot is the regression test for the
// __abRoots memoization. The async wait/assert poll loops must recompute the
// frame/shadow root list on every tick, so an element inside a shadow root (or
// same-origin iframe) attached PART WAY THROUGH a wait is still discovered. A
// frozen root cache — the bug an earlier memoization introduced — would snapshot
// the root list on the first poll, never see the late shadow root, and time out.
func TestAssertFindsRefInLateAttachedShadowRoot(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// The target (with a pre-set ref) lives in a shadow root attached ~400ms after
	// load — after the first assert poll has already run and would have cached the
	// root list without it.
	html := `<div id="host"></div><script>
setTimeout(function(){
  var sr = document.getElementById('host').attachShadow({mode:'open'});
  sr.innerHTML = '<button data-brw-ref="e999" style="position:fixed;left:10px;top:10px;width:90px;height:30px">Late</button>';
}, 400);
</script>`
	openHTMLInManager(t, m, ctx, html)

	if err := m.AssertVisible(ctx, "e999", 5*time.Second); err != nil {
		t.Fatalf("AssertVisible for a ref in a late-attached shadow root failed: %v (a frozen __abRoots cache in the poll loop would never find it)", err)
	}
}
