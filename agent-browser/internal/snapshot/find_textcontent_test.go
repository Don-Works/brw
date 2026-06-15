package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestFindTextContentMatchesProse proves SPEC 2: browser_find with TextContent
// enabled matches visible prose text (not just interactive-element metadata),
// while the default (TextContent off) does not surface prose-only elements.
func TestFindTextContentMatchesProse(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<h1>Glossary</h1>
<p id="def">HyperText is text displayed on a device with references to other text.</p>
<a href="/spec">Specification</a>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	// Default find (no text_content): the prose word "HyperText" lives only in a
	// <p>'s visible text, not in any interactive element's name/attributes, so it
	// must NOT be found. This guards the opt-in contract.
	base, err := Find(ctx, FindOptions{Query: "HyperText"})
	if err != nil {
		t.Fatalf("find (default): %v", err)
	}
	if len(base.Elements) != 0 {
		t.Fatalf("default find unexpectedly matched prose; got %d elements: %v", len(base.Elements), names(base.Elements))
	}

	// With text_content on, the prose is searchable and the matching element is
	// returned with a "text" match reason.
	withText, err := Find(ctx, FindOptions{Query: "HyperText", TextContent: true})
	if err != nil {
		t.Fatalf("find (text_content): %v", err)
	}
	if len(withText.Elements) == 0 {
		t.Fatalf("text_content find did not match prose 'HyperText'")
	}
	var hasTextReason bool
	for _, el := range withText.Elements {
		for _, r := range el.MatchReasons {
			if r == "text" {
				hasTextReason = true
			}
		}
	}
	if !hasTextReason {
		t.Fatalf("expected a 'text' match_reason on a text_content match; elements: %+v", withText.Elements)
	}
}

func names(els []Element) []string {
	out := make([]string, 0, len(els))
	for i := range els {
		out = append(out, els[i].Name)
	}
	return out
}
