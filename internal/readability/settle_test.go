package readability

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestReadSettleBudgetControlsDeferredContent(t *testing.T) {
	ctx, cancel := readTestContext(t)
	defer cancel()
	for _, budget := range []int{0, 800, 1500} {
		var read PageRead
		expr := `(async function() {
			document.body.innerHTML = '<main>Ready</main>';
			var timer = setTimeout(function() {
				document.querySelector('main').textContent = 'Deferred content is now available with enough useful text to finish the read.';
			}, 100);
			try { return await ` + ReadExpr(budget) + `; }
			finally { clearTimeout(timer); }
		})()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &read, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		})); err != nil {
			t.Fatal(err)
		}
		if budget == 0 && read.Main != "Ready" {
			t.Fatalf("immediate read=%q", read.Main)
		}
		if budget > 0 && !strings.Contains(read.Main, "Deferred content") {
			t.Fatalf("budget %d did not wait for content: %q", budget, read.Main)
		}
	}
}

func TestReadSettleValidation(t *testing.T) {
	for _, ms := range []int{-1, 0, 800, 5000, 5001} {
		err := (ReadOptions{SettleMS: &ms}).Validate()
		if (err == nil) != (ms >= 0 && ms <= 5000) {
			t.Fatalf("budget=%d err=%v", ms, err)
		}
	}
}
