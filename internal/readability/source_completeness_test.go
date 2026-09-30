package readability

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestReadSourceCompleteness(t *testing.T) {
	ctx, cancel := readTestContext(t)
	defer cancel()
	padding := strings.Repeat("Ordinary body prose. ", 15)
	cases := []struct {
		name  string
		html  string
		check func(*testing.T, PageRead)
	}{
		{"unique headings", "<main><h1>Article</h1><p>" + padding + "</p><h2>Install</h2><p>Run it.</p></main>", func(t *testing.T, read PageRead) {
			if !read.SectionsAnchored || len(read.Headings) != 2 || read.Headings[1].Offset == nil || *read.Headings[1].Offset < 0 {
				t.Fatalf("missing exact anchors: %+v", read)
			}
		}},
		{"prose mentions heading", "<main><h1>Article</h1><p>See Install below. " + padding + "</p><h2>Install</h2><p>Run it.</p></main>", func(t *testing.T, read PageRead) {
			if read.SectionsAnchored {
				t.Fatal("ambiguous prose occurrence certified as anchored")
			}
		}},
		{"outside heading matches prose", "<nav><h2>Install</h2></nav><main><h1>Article</h1><p>See Install elsewhere. " + padding + "</p></main>", func(t *testing.T, read PageRead) {
			if len(read.Headings) != 2 || read.Headings[0].Offset == nil || *read.Headings[0].Offset != -1 || !read.SectionsAnchored {
				t.Fatalf("outside heading acquired an anchor: %+v", read)
			}
		}},
		{"empty rows before truncation", "<main>" + padding + "<table><caption>Rows</caption>" + strings.Repeat("<tr></tr>", 40) + "<tr><td>omitted</td></tr></table></main>", func(t *testing.T, read PageRead) {
			if !read.TablesComplete || len(read.Tables) != 1 || !read.Tables[0].Truncated || len(read.Tables[0].Rows) != 0 {
				t.Fatalf("missing row truncation evidence: %+v", read.Tables)
			}
		}},
		{"table inventory truncated", "<main>" + padding + strings.Repeat("<table><tr><td>data</td></tr></table>", 21) + "</main>", func(t *testing.T, read PageRead) {
			if read.TablesComplete || !read.TablesTruncated || len(read.Tables) != 20 {
				t.Fatal("incomplete table inventory reported as complete")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.html)
			if err != nil {
				t.Fatal(err)
			}
			var read PageRead
			expr := "(async function(){document.body.innerHTML=" + string(body) + ";return await " + ReadExpr(0) + ";})()"
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &read, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
				t.Fatal(err)
			}
			tc.check(t, read)
		})
	}
}

func TestReadWindowDoesNotCertifyOmittedSources(t *testing.T) {
	read := PageRead{Main: "Section body", SectionsAnchored: true, TablesComplete: true, Headings: []Heading{{Text: "Section", Level: 1, Offset: intPtr(0)}}}
	for _, opts := range []ReadOptions{{Include: []string{"main"}}, {Include: []string{"tables"}}, {MaxChars: 1}, {Section: "Section"}} {
		out := Window(read, opts)
		if out.SectionsAnchored {
			t.Fatalf("bounded or omitted source still certified: %+v", opts)
		}
		if !opts.wants("tables") && out.TablesComplete {
			t.Fatal("omitted tables still certified complete")
		}
	}
}
