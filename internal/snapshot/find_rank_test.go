package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestFindPrefersTextboxOverLabel proves the agent footgun fix: a form with a
// <label>Username</label> before <input name=Username> must resolve
// find{query:"username", limit:1} to the textbox, not the label. Without
// match ranking, DOM order made fill-by-query write into the label.
func TestFindPrefersTextboxOverLabel(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<label for="username">Username</label>
<input id="username" name="username" type="text" />
<label for="password">Password</label>
<input id="password" name="password" type="password" />
<button type="submit">Login</button>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	found, err := Find(ctx, FindOptions{Query: "username", Limit: 1})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found.Elements) == 0 {
		t.Fatal("find returned no elements")
	}
	el := found.Elements[0]
	if el.Role != "textbox" && el.Tag != "input" {
		t.Fatalf("find{query:username,limit:1} returned role=%q tag=%q name=%q; want textbox/input (not label)",
			el.Role, el.Tag, el.Name)
	}
}

// TestFindDoesNotReportLowCoverageOnNarrowQuery guards against the false
// low_semantic_coverage signal on filtered finds — a single match on a rich
// page is a narrow query, not a sparse CSR surface.
func TestFindDoesNotReportLowCoverageOnNarrowQuery(t *testing.T) {
	body := ""
	for i := 0; i < 20; i++ {
		body += "<div>Lorem ipsum dolor sit amet consectetur adipiscing elit.</div>"
	}
	body += `<label>Username</label><input id="u" type="text" name="username" />`
	for i := 0; i < 10; i++ {
		body += `<button>Btn ` + string(rune('A'+i)) + `</button>`
	}
	html := `<!DOCTYPE html><html><body>` + body + `</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	found, err := Find(ctx, FindOptions{Query: "username", Limit: 5})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if low, _ := found.Metadata["low_semantic_coverage"].(bool); low {
		t.Fatalf("narrow find should not set low_semantic_coverage; metadata=%v", found.Metadata)
	}
}
