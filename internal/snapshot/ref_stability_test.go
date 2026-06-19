package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

func refByHref(els []Element, href string) string {
	for i := range els {
		if els[i].Href == href || (len(els[i].Href) >= len(href) && els[i].Href[len(els[i].Href)-len(href):] == href) {
			return els[i].Ref
		}
	}
	return ""
}

// TestRefStableAcrossReRenderWithTextChange proves SPEC 4: a link identified by a
// stable href keeps its ref across a SPA-style re-render that (a) REPLACES the DOM
// node (dropping the stamped data-brw-ref attribute), (b) changes the
// element's visible text, and (c) reorders siblings. Before the fix the key
// embedded mutable innerText + nth-of-type position, so the new node computed a
// different key and was assigned a fresh ref.
func TestRefStableAcrossReRenderWithTextChange(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<nav id="nav">
  <a href="/docs">Docs</a>
  <a href="/blog">Blog</a>
  <a href="/about">About</a>
</nav>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	first, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	blogRef := refByHref(first.Elements, "/blog")
	docsRef := refByHref(first.Elements, "/docs")
	if blogRef == "" || docsRef == "" {
		t.Fatalf("did not surface /blog and /docs links; elements: %v", names(first.Elements))
	}

	// Re-render: brand new nodes (innerHTML wipe), changed link text, reordered
	// siblings. hrefs are preserved — that is the stable identity.
	rerender := `(function(){
		document.getElementById('nav').innerHTML =
			'<a href="/about">About Us</a>' +
			'<a href="/blog">Read the Blog</a>' +
			'<a href="/docs">Documentation</a>';
		return true;
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(rerender, nil)); err != nil {
		t.Fatalf("re-render: %v", err)
	}

	second, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if got := refByHref(second.Elements, "/blog"); got != blogRef {
		t.Fatalf("/blog ref renumbered across re-render: was %q, now %q", blogRef, got)
	}
	if got := refByHref(second.Elements, "/docs"); got != docsRef {
		t.Fatalf("/docs ref renumbered across re-render: was %q, now %q", docsRef, got)
	}
}

// TestRefDistinctSiblingsWithoutStableIdentityDoNotCollapse guards the collision
// risk in the SPEC 4 fix: two genuinely distinct elements that share tag+role and
// carry NO stable identity attribute (no id/name/href/aria-label) must still get
// DISTINCT refs. For these, stableKeyFor returns ” and refFor falls back to the
// legacy text+path key, preserving today's behavior.
func TestRefDistinctSiblingsWithoutStableIdentityDoNotCollapse(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<button>Alpha</button>
<button>Beta</button>
<button>Gamma</button>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	snap, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	refs := map[string]bool{}
	buttons := 0
	for _, el := range snap.Elements {
		if el.Tag != "button" {
			continue
		}
		buttons++
		if refs[el.Ref] {
			t.Fatalf("two distinct buttons collapsed to the same ref %q; elements: %+v", el.Ref, snap.Elements)
		}
		refs[el.Ref] = true
	}
	if buttons != 3 {
		t.Fatalf("expected 3 buttons, got %d", buttons)
	}
}
