package snapshot

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func node(role, name string, children ...AriaNode) AriaNode {
	return AriaNode{Role: role, Name: name, Children: children}
}

func TestDiffAriaTreesFindsRoleAndNameChanges(t *testing.T) {
	base := AriaTree{Nodes: []AriaNode{node("main", "",
		node("heading", "Invoices"),
		node("button", "Pay invoice"),
	)}}

	cases := []struct {
		name   string
		after  AriaTree
		kind   string
		from   string
		to     string
		count  int
		noDiff bool
	}{
		{name: "identical", after: base, noDiff: true},
		{
			name:  "a button loses its accessible name",
			after: AriaTree{Nodes: []AriaNode{node("main", "", node("heading", "Invoices"), node("button", ""))}},
			kind:  AriaChangeNameChanged, from: "Pay invoice", to: "", count: 1,
		},
		{
			name:  "a control changes role",
			after: AriaTree{Nodes: []AriaNode{node("main", "", node("heading", "Invoices"), node("link", "Pay invoice"))}},
			kind:  AriaChangeRoleChanged, from: "button", to: "link", count: 1,
		},
		{
			name:  "a control disappears",
			after: AriaTree{Nodes: []AriaNode{node("main", "", node("heading", "Invoices"))}},
			kind:  AriaChangeRemoved, from: "Pay invoice", count: 1,
		},
		{
			name:  "a control appears",
			after: AriaTree{Nodes: []AriaNode{node("main", "", node("heading", "Invoices"), node("button", "Pay invoice"), node("button", "Cancel"))}},
			kind:  AriaChangeAdded, to: "Cancel", count: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := DiffAriaTrees(base, tc.after)
			if tc.noDiff {
				if diff.Changed || diff.Count != 0 {
					t.Fatalf("diff = %+v, want no change", diff)
				}
				return
			}
			if !diff.Changed || diff.Count != tc.count {
				t.Fatalf("diff = %+v, want %d change(s)", diff, tc.count)
			}
			change := diff.Changes[0]
			if change.Kind != tc.kind || change.From != tc.from || change.To != tc.to {
				t.Fatalf("change = %+v, want kind %q from %q to %q", change, tc.kind, tc.from, tc.to)
			}
		})
	}
}

// Sibling paths must not bleed into each other: two buttons under the same
// parent have to be distinguishable in the report.
func TestAriaDiffPathsDistinguishSiblings(t *testing.T) {
	before := AriaTree{Nodes: []AriaNode{
		node("form", "", node("button", "Save")),
		node("form", "", node("button", "Delete")),
	}}
	after := AriaTree{Nodes: []AriaNode{
		node("form", "", node("button", "Save")),
		node("form", "", node("button", "Remove")),
	}}
	diff := DiffAriaTrees(before, after)
	if diff.Count != 1 {
		t.Fatalf("diff = %+v, want one change", diff)
	}
	if got := diff.Changes[0].Path; got != "form[1] > button[0]" {
		t.Fatalf("path = %q, want it to name the SECOND form's button", got)
	}
}

func TestAriaDiffCapsTheListButNotTheCount(t *testing.T) {
	var before, after AriaTree
	for i := 0; i < MaxAriaChanges+10; i++ {
		before.Nodes = append(before.Nodes, node("button", "before"))
		after.Nodes = append(after.Nodes, node("button", "after"))
	}
	diff := DiffAriaTrees(before, after)
	if diff.Count != MaxAriaChanges+10 {
		t.Fatalf("count = %d, want the exact number of changes", diff.Count)
	}
	if len(diff.Changes) != MaxAriaChanges || !diff.Truncated {
		t.Fatalf("changes = %d truncated = %v, want the list capped and said so", len(diff.Changes), diff.Truncated)
	}
}

func TestParseAriaTreeRejectsNothing(t *testing.T) {
	if _, err := ParseAriaTree(nil); err == nil {
		t.Fatal("a page that returned nothing must be an error, not an empty tree that compares as a match")
	}
}

// The script has to work in a real browser, because that is the only place it
// ever runs. Skipped when no local Chrome exists.
func TestAriaTreeScriptReadsRolesAndNamesFromARealPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>ARIA Fixture</title>
<main>
  <div class="wrapper"><div class="inner">
    <h1>Invoices</h1>
    <button aria-label="Pay invoice">OK</button>
    <a href="/help">Help</a>
    <input id="q" type="search">
    <label for="q">Search invoices</label>
    <span aria-hidden="true"><button>Hidden from a11y</button></span>
    <button style="display:none">Not rendered</button>
  </div></div>
</main>`))
	}))
	defer srv.Close()

	ctx, cancel := structuredTestContext(t)
	defer cancel()

	var raw any
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.Evaluate(AriaTreeExpression, &raw),
	); err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	tree, err := ParseAriaTree(raw)
	if err != nil {
		t.Fatalf("ParseAriaTree: %v", err)
	}
	if len(tree.Nodes) != 1 || tree.Nodes[0].Role != "main" {
		t.Fatalf("tree roots = %+v, want a single main landmark", tree.Nodes)
	}
	// The two presentational wrapper divs must be flattened away: a CSS
	// refactor that adds or removes one is not a regression.
	children := tree.Nodes[0].Children
	got := map[string]string{}
	for _, child := range children {
		got[child.Role] = child.Name
	}
	for role, want := range map[string]string{
		"heading":   "Invoices",
		"button":    "Pay invoice",
		"link":      "Help",
		"searchbox": "Search invoices",
	} {
		if got[role] != want {
			t.Errorf("role %q name = %q, want %q (tree: %+v)", role, got[role], want, children)
		}
	}
	for _, child := range children {
		if child.Name == "Hidden from a11y" || child.Name == "Not rendered" {
			t.Errorf("an aria-hidden or display:none control reached the tree: %+v", child)
		}
	}
	if tree.Truncated {
		t.Fatal("a six-element fixture must not hit the node cap")
	}
}

// countStyleReads replaces window.getComputedStyle with a counting wrapper, so
// a test can measure the forced style reads the walk causes instead of timing
// it. The walk runs on every baseline check, on whatever page the recipe left
// open.
const countStyleReads = `(function(){
  var real = window.getComputedStyle;
  window.__brwStyleReads = 0;
  window.getComputedStyle = function(){ window.__brwStyleReads++; return real.apply(window, arguments); };
  return true;
})()`

// MAX_NODES caps the emitted nodes. It has to cap the traversal too: a role-less
// wrapper is recursed into without incrementing the count, so a walk that only
// returned from one level would keep reading styles across the rest of the
// document to build nodes it then discards. And an element with no role and no
// element children contributes nothing either way, so it must never cost a
// style read at all.
func TestAriaTreeWalkDoesNotReadStylesItCannotUse(t *testing.T) {
	var presentational strings.Builder
	for i := 0; i < 500; i++ {
		presentational.WriteString(`<span class="pres">text</span>`)
	}
	var overCap strings.Builder
	for i := 0; i < 2100; i++ {
		fmt.Fprintf(&overCap, `<div class="w"><button>Button %d</button></div>`, i)
	}
	// The tail sits after the node cap is reached and is nested, so the
	// role-less-leaf shortcut cannot account for it: only stopping the walk can.
	for i := 0; i < 5000; i++ {
		overCap.WriteString(`<div class="tail"><span>tail</span></div>`)
	}

	cases := []struct {
		name          string
		body          string
		maxStyleReads int
		wantNodes     int
		wantTruncated bool
	}{
		{
			name:          "presentational leaves cost nothing",
			body:          `<main>` + presentational.String() + `<button>Pay invoice</button></main>`,
			maxStyleReads: 20,
			wantNodes:     2,
		},
		{
			name:          "the node cap stops the walk",
			body:          `<main>` + overCap.String() + `</main>`,
			maxStyleReads: 5000,
			wantNodes:     2000,
			wantTruncated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/html")
				_, _ = w.Write([]byte(`<!doctype html><title>Walk Cost Fixture</title>` + tc.body))
			}))
			defer srv.Close()

			ctx, cancel := structuredTestContext(t)
			defer cancel()

			var installed bool
			var raw any
			var reads int
			if err := chromedp.Run(ctx,
				chromedp.Navigate(srv.URL),
				chromedp.Evaluate(countStyleReads, &installed),
				chromedp.Evaluate(AriaTreeExpression, &raw),
				chromedp.Evaluate(`window.__brwStyleReads`, &reads),
			); err != nil {
				t.Skipf("headless Chrome unavailable: %v", err)
			}
			if !installed {
				t.Fatal("the getComputedStyle counter was not installed")
			}
			tree, err := ParseAriaTree(raw)
			if err != nil {
				t.Fatalf("ParseAriaTree: %v", err)
			}
			if tree.Count() != tc.wantNodes {
				t.Fatalf("tree holds %d nodes, want %d", tree.Count(), tc.wantNodes)
			}
			if tree.Truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", tree.Truncated, tc.wantTruncated)
			}
			if reads > tc.maxStyleReads {
				t.Fatalf("the walk forced %d style reads, want at most %d", reads, tc.maxStyleReads)
			}
		})
	}
}

// The editable HOST is the one textbox. Naming every descendant of an editor a
// textbox makes an ordinary paragraph edit read as a structural regression, and
// the inherited-editability property that produced those nodes forces a style
// update on every element the walk asks.
func TestAriaTreeNamesTheContentEditableHostOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Editable Fixture</title>
<main>
  <div contenteditable="true" aria-label="Message body"><p>one</p><p>two</p></div>
  <div contenteditable="false"><p>not editable</p></div>
</main>`))
	}))
	defer srv.Close()

	ctx, cancel := structuredTestContext(t)
	defer cancel()

	var raw any
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.Evaluate(AriaTreeExpression, &raw),
	); err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}
	tree, err := ParseAriaTree(raw)
	if err != nil {
		t.Fatalf("ParseAriaTree: %v", err)
	}
	if len(tree.Nodes) != 1 || tree.Nodes[0].Role != "main" {
		t.Fatalf("tree roots = %+v, want a single main landmark", tree.Nodes)
	}
	children := tree.Nodes[0].Children
	if len(children) != 1 {
		t.Fatalf("main holds %+v, want only the editable host", children)
	}
	if children[0].Role != "textbox" || children[0].Name != "Message body" {
		t.Fatalf("editable host = %+v, want a named textbox", children[0])
	}
	if len(children[0].Children) != 0 {
		t.Fatalf("the editor's paragraphs reached the tree as %+v", children[0].Children)
	}
}
