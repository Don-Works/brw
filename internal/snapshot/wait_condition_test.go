package snapshot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// serveWaitFixture publishes body at "/" and navigates ctx to it.
func serveWaitFixture(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
}

// A selector / fn condition must be satisfied by state that only appears after
// the wait has already started, which is the case that distinguishes a real
// event-driven wait from a check that happens to run late.
const deferredAppearFixture = `<!doctype html><html><body>
<div id="host"></div>
<script>
  setTimeout(function(){
    var d = document.createElement('div');
    d.className = 'ready';
    d.id = 'late';
    d.textContent = 'loaded';
    document.getElementById('host').appendChild(d);
    window.__brwTestFlag = true;
  }, 150);
</script>
</body></html>`

func TestWaitForConditionSelectorAndFn(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		condition string
		timeoutMS int64
		want      bool
		wantErr   string
	}{
		{
			name:      "selector matches an element added after the wait starts",
			fixture:   deferredAppearFixture,
			condition: "selector:.ready",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "selector times out when nothing ever matches",
			fixture:   deferredAppearFixture,
			condition: "selector:.never-appears",
			timeoutMS: 300,
			want:      false,
		},
		{
			name:      "not_selector is immediately true for an absent element",
			fixture:   deferredAppearFixture,
			condition: "not_selector:.never-appears",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "not_selector becomes true once the element is removed",
			fixture:   `<!doctype html><html><body><div class="spinner"></div><script>setTimeout(function(){document.querySelector('.spinner').remove();},150);</script></body></html>`,
			condition: "not_selector:.spinner",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "fn expression resolves on the mutation that makes it true",
			fixture:   deferredAppearFixture,
			condition: "fn:document.querySelector('.ready') !== null",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "fn sees a page global set by the page's own script",
			fixture:   deferredAppearFixture,
			condition: "fn:window.__brwTestFlag === true",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "fn statement body with an explicit return is accepted",
			fixture:   deferredAppearFixture,
			condition: "fn:const n = document.querySelectorAll('div').length; return n > 1;",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "fn that throws while waiting is treated as not-yet, not a failure",
			fixture:   deferredAppearFixture,
			condition: "fn:document.getElementById('late').textContent === 'loaded'",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "async fn is awaited",
			fixture:   deferredAppearFixture,
			condition: "fn:(async () => document.querySelector('.ready') !== null)()",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "fn that never becomes true times out",
			fixture:   deferredAppearFixture,
			condition: "fn:window.__nope === 42",
			timeoutMS: 300,
			want:      false,
		},
		{
			name:      "fn that cannot compile reports its own syntax error",
			fixture:   deferredAppearFixture,
			condition: "fn:this is not javascript(((",
			timeoutMS: 1000,
			wantErr:   "wait fn did not compile",
		},
		{
			name:      "existing ready condition still works",
			fixture:   deferredAppearFixture,
			condition: "ready",
			timeoutMS: 5000,
			want:      true,
		},
		{
			name:      "existing text condition still works",
			fixture:   deferredAppearFixture,
			condition: "text:loaded",
			timeoutMS: 5000,
			want:      true,
		},
	}

	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveWaitFixture(t, ctx, tt.fixture)
			matched, err := WaitForCondition(ctx, tt.condition, tt.timeoutMS)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (matched=%v)", tt.wantErr, matched)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WaitForCondition(%q): %v", tt.condition, err)
			}
			if matched != tt.want {
				t.Fatalf("WaitForCondition(%q) = %v, want %v", tt.condition, matched, tt.want)
			}
		})
	}
}

// The point of compiling fn: once and re-running it from the MutationObserver is
// that it resolves on the mutation, not on the next 100ms poll tick. A predicate
// satisfied ~150ms in must therefore return well before the 5s timeout, and
// close to the mutation itself.
func TestWaitForConditionFnIsEventDrivenNotPolled(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	serveWaitFixture(t, ctx, deferredAppearFixture)

	start := time.Now()
	matched, err := WaitForCondition(ctx, "fn:document.querySelector('.ready') !== null", 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForCondition: %v", err)
	}
	if !matched {
		t.Fatal("condition should have matched")
	}
	// The fixture mutates at 150ms. A polled implementation lands on a 100ms
	// boundary; an event-driven one resolves on the mutation. Allow generous
	// headroom for CI while still failing a full-timeout regression.
	if elapsed > 2*time.Second {
		t.Fatalf("fn condition took %v; expected to resolve on the mutation at ~150ms", elapsed)
	}
}
