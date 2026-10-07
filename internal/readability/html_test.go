package readability

import (
	"strings"
	"testing"
)

func TestHTMLPreservesInlineWordBoundaries(t *testing.T) {
	read, err := FromHTML("https://example.test/", []byte(`<main><p>Run <code>brw</code> now.</p><p><span>one</span> <em>two</em></p><p>un<strong>break</strong>able</p></main>`))
	if err != nil {
		t.Fatal(err)
	}
	if read.Main != "Run brw now.\none two\nunbreakable" {
		t.Fatalf("word boundaries changed: %q", read.Main)
	}
}

func TestHTMLPreservesHighlightedCode(t *testing.T) {
	html := "<main><h1>Commands</h1><pre><code><span>brw</span><span> open </span>https://example.test/\n  brw read\n\n\n  literal ``` fence  \n</code></pre><h2>Next</h2><p>Continue.</p></main>"
	read, err := FromHTML("https://example.test/", []byte(html))
	if err != nil {
		t.Fatal(err)
	}
	want := "````\nbrw open https://example.test/\n  brw read\n\n\n  literal ``` fence  \n````"
	if !strings.Contains(read.Main, want) {
		t.Fatalf("code changed: %q", read.Main)
	}
	if len(read.Headings) != 2 || !strings.Contains(read.Main, "## Next\nContinue.") {
		t.Fatalf("heading extraction changed: %+v", read)
	}
}

func TestHTMLPreservesCodeWithoutMainLandmark(t *testing.T) {
	read, err := FromHTML("https://example.test/", []byte("<body><div><pre><code>first\n  second</code></pre></div></body>"))
	if err != nil {
		t.Fatal(err)
	}
	if read.Main != "```\nfirst\n  second\n```" {
		t.Fatalf("code changed: %q", read.Main)
	}
}

func TestMarkdownTitleUsesRealHeadings(t *testing.T) {
	for _, tc := range []struct{ title, markdown, want string }{
		{"", "```\n# Code example\n```\n\n## Article\nBody", "Article"},
		{"", "#hashtag\n\n# Article\nBody", "Article"},
		{"Provided title", "# Article", "Provided title"},
	} {
		if got := FromMarkdown("https://example.test/", tc.title, tc.markdown); got.Title != tc.want {
			t.Errorf("title=%q, want %q", got.Title, tc.want)
		}
	}
}
