package readability

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func readTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	// Warm up so allocator/browser errors surface here and the test can skip
	// cleanly when no Chrome is available.
	if err := chromedp.Run(timeoutCtx); err != nil {
		timeoutCancel()
		cancel()
		allocCancel()
		t.Skipf("headless Chrome not available: %v", err)
	}
	return timeoutCtx, func() {
		timeoutCancel()
		cancel()
		allocCancel()
	}
}

func navigateRead(t *testing.T, ctx context.Context, html string) PageRead {
	t.Helper()
	if err := chromedp.Run(ctx,
		chromedp.Navigate("data:text/html,"+url.PathEscape(html)),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	read, err := Evaluate(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return read
}

// TestReadLinkHeavyPageFallsBackToDocumentText proves SPEC 3: on a link-heavy
// page (Hacker News / Wikipedia-category shape — nested divs, almost all visible
// text is links, no <article>/<main>) the link-text penalty in bestMain() drives
// the scored main element to <body>, and historically .main came back empty
// because text(mainEl) was effectively blank while links extracted fine. The
// two-tier fallback must now return non-empty document text.
func TestReadLinkHeavyPageFallsBackToDocumentText(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<div><div>
  <span>1.</span> <a href="/a">First story headline goes here</a> <span>(example.com)</span>
  <div><span>120 points by alice 2 hours ago</span> | <a href="/c1">discuss</a></div>
  <span>2.</span> <a href="/b">Second story headline appears now</a> <span>(test.org)</span>
  <div><span>88 points by bob 3 hours ago</span> | <a href="/c2">comments</a></div>
</div></div>
</body></html>`

	ctx, cancel := readTestContext(t)
	defer cancel()
	read := navigateRead(t, ctx, html)

	if strings.TrimSpace(read.Main) == "" {
		t.Fatalf("link-heavy page returned empty .main despite visible text; links=%d", len(read.Links))
	}
	if !strings.Contains(read.Main, "First story headline") {
		t.Fatalf(".main fallback did not capture visible text; got %q", read.Main)
	}
	if len(read.Links) == 0 {
		t.Fatalf("expected links to populate (they already did before the fix)")
	}
}

// TestReadCSRPageWaitsForDeferredContent proves SPEC 2: a heavy client-side
// rendered page serves a near-empty shell on first paint and streams the real
// content in milliseconds later. A single synchronous extract returns blank
// .main; the brief CSR settle wait must observe the deferred DOM mutation and
// return the populated content rather than empty.
func TestReadCSRPageWaitsForDeferredContent(t *testing.T) {
	// The body is empty at load; a microtask-delayed script injects the real
	// article text after a short delay, simulating an SPA hydration/render.
	html := `<!DOCTYPE html><html><head><title>CSR App</title></head><body>
<div id="app"></div>
<script>
setTimeout(function(){
  var a = document.getElementById('app');
  a.innerHTML = '<main><h1>Hydrated Heading</h1>' +
    '<p>This article body was rendered entirely on the client after initial paint, the way a single-page application hydrates its content into an empty shell element.</p>' +
    '<p>A second client-rendered paragraph adds enough prose that the main extractor clears its usefulness threshold once the content actually exists in the DOM.</p></main>';
}, 120);
</script>
</body></html>`

	ctx, cancel := readTestContext(t)
	defer cancel()
	read := navigateRead(t, ctx, html)

	if strings.TrimSpace(read.Main) == "" {
		t.Fatalf("CSR page returned empty .main; the settle wait must capture deferred client-rendered content")
	}
	if !strings.Contains(read.Main, "rendered entirely on the client") {
		t.Fatalf(".main did not capture the client-rendered content; got %q", read.Main)
	}
}

// TestReadArticlePagePreservesSemanticExtraction is the regression guard: a
// well-formed article must still extract its semantic <article> body and must not
// be diluted by the fallback (the fallback only fires when primary text < 50
// chars).
func TestReadArticlePagePreservesSemanticExtraction(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<nav><a href="/x">Nav One</a> <a href="/y">Nav Two</a></nav>
<article>
  <h1>The Title Of The Article</h1>
  <p>This is the first substantial paragraph of the article body. It contains enough prose to clear the semantic-main threshold comfortably and should be selected as the main content.</p>
  <p>A second paragraph continues the discussion with more meaningful sentences so the article element clearly wins the scoring over the navigation chrome.</p>
</article>
</body></html>`

	ctx, cancel := readTestContext(t)
	defer cancel()
	read := navigateRead(t, ctx, html)

	if !strings.Contains(read.Main, "first substantial paragraph") {
		t.Fatalf(".main did not extract the article body; got %q", read.Main)
	}
	if strings.Contains(read.Main, "Nav One") {
		t.Fatalf(".main leaked navigation chrome (fallback fired on a good article?); got %q", read.Main)
	}
}
