package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestIsExternalRefClassifiesEveryReferenceShape enumerates the forms a
// reference takes. The ones that matter are the scheme-relative "//host/x",
// which a check for "http" walks straight past, and the uppercase scheme, which
// a case-sensitive one does.
func TestIsExternalRefClassifiesEveryReferenceShape(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{ref: "", want: false},
		{ref: "style.css", want: false},
		{ref: "/assets/style.css", want: false},
		{ref: "../shared/app.js", want: false},
		{ref: "#main", want: false},
		{ref: "data:image/png;base64,AAAA", want: false},
		{ref: "blob:http://127.0.0.1:1/abc", want: false},
		{ref: "about:blank", want: false},
		{ref: "http://127.0.0.1:8080/x.css", want: false},
		{ref: "http://localhost:8080/x.css", want: false},
		{ref: "https://[::1]:8080/x.css", want: false},
		{ref: "mailto:someone@example.test", want: false},
		{ref: "https://cdn.example.test/x.css", want: true},
		{ref: "http://cdn.example.test/x.js", want: true},
		{ref: "HTTPS://CDN.EXAMPLE.TEST/x.css", want: true},
		{ref: "//cdn.example.test/x.css", want: true},
		{ref: "wss://realtime.example.test/socket", want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.ref, func(t *testing.T) {
			if got := IsExternalRef(testCase.ref); got != testCase.want {
				t.Fatalf("IsExternalRef(%q) = %v, want %v", testCase.ref, got, testCase.want)
			}
		})
	}
}

// TestExternalResourceRefsFindsWhatThePageWouldFetch covers the attributes a
// page loads through, and the one it does not: a link the harness never clicks
// is not a fetch, and flagging it would make the audit unusable against
// fixtures that illustrate links.
func TestExternalResourceRefsFindsWhatThePageWouldFetch(t *testing.T) {
	page := `<!doctype html>
<html>
  <head>
    <link rel="stylesheet" href="https://cdn.example.test/site.css">
    <link rel="icon" href="/favicon.ico">
    <link rel="canonical" href="https://example.test/the/page">
    <link rel="preconnect" href="https://connect.example.test">
    <link rel="something-invented-later" href="https://future.example.test/x">
    <style>@import "https://fonts.example.test/face.css"; body { background: url('//img.example.test/bg.png'); }</style>
  </head>
  <body>
    <img src="local.png">
    <img SRC="https://img.example.test/hero.png">
    <script src="app.js"></script>
    <a href="https://docs.example.test/guide">an illustrative link nothing clicks</a>
    <form action="https://forms.example.test/submit"></form>
    <video poster="//cdn.example.test/poster.jpg"></video>
  </body>
</html>`
	path := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}

	refs, err := ExternalResourceRefs(path)
	if err != nil {
		t.Fatal(err)
	}
	found := strings.Join(refs, " ")
	for _, want := range []string{
		"https://cdn.example.test/site.css",
		"https://fonts.example.test/face.css",
		"//img.example.test/bg.png",
		"https://img.example.test/hero.png",
		"https://forms.example.test/submit",
		"//cdn.example.test/poster.jpg",
		// preconnect opens a connection without fetching a document, and a rel
		// invented after this code was written has to be flagged rather than
		// waved through: the audit exempts a closed list and flags the rest.
		"https://connect.example.test",
		"https://future.example.test/x",
	} {
		if !strings.Contains(found, want) {
			t.Errorf("audit missed %s; found %v", want, refs)
		}
	}
	for _, unwanted := range []string{"local.png", "app.js", "/favicon.ico",
		"https://docs.example.test/guide", "https://example.test/the/page"} {
		if strings.Contains(found, unwanted) {
			t.Errorf("audit flagged %s, which the page does not fetch off this machine; found %v", unwanted, refs)
		}
	}

	if err := AuditFixtures([]string{path}); err == nil {
		t.Fatal("AuditFixtures passed a page that loads from six other hosts")
	}
}

// TestExternalResourceRefsCatchesEveryWayPastTheTextScan is the table the regex
// audit could not satisfy. Every "flagged" row below went through the old scan
// clean — unquoted values had no quotes to match, href outside <link> was never
// read, and CSS and JavaScript inside the page were not looked at — so each one
// was a fixture edit away from putting somebody else's network inside a
// deterministic run.
//
// The "ignored" rows are the other half: an audit that cried wolf on an
// illustrative <a href>, a JSON-LD @context or a local srcset is an audit that
// gets switched off.
func TestExternalResourceRefsCatchesEveryWayPastTheTextScan(t *testing.T) {
	cases := []struct {
		name    string
		markup  string
		flagged string
	}{
		{name: "object data", markup: `<object data="https://evil.test/x.pdf"></object>`, flagged: "https://evil.test/x.pdf"},
		{name: "svg use href", markup: `<svg><use href="https://evil.test/sprite.svg#icon"></use></svg>`, flagged: "https://evil.test/sprite.svg#icon"},
		{name: "svg image href", markup: `<svg><image href="https://evil.test/pic.png"></image></svg>`, flagged: "https://evil.test/pic.png"},
		{name: "svg xlink href", markup: `<svg><use xlink:href="https://evil.test/legacy.svg#icon"></use></svg>`, flagged: "https://evil.test/legacy.svg#icon"},
		{name: "meta refresh", markup: `<meta http-equiv="refresh" content="0;url=https://evil.test/next">`, flagged: "https://evil.test/next"},
		{name: "unquoted src", markup: `<img src=https://evil.test/unquoted.png>`, flagged: "https://evil.test/unquoted.png"},
		{name: "srcdoc iframe", markup: `<iframe srcdoc="&lt;img src=&quot;https://evil.test/inner.png&quot;&gt;"></iframe>`, flagged: "https://evil.test/inner.png"},
		{name: "inline fetch", markup: `<script>fetch("https://evil.test/beacon")</script>`, flagged: "https://evil.test/beacon"},
		{name: "inline dynamic import", markup: `<script type="module">import("https://evil.test/mod.js")</script>`, flagged: "https://evil.test/mod.js"},
		{name: "inline scheme-relative", markup: `<script>new Image().src = "//evil.test/px.gif"</script>`, flagged: "//evil.test/px.gif"},
		{name: "srcset candidate", markup: `<img srcset="local.png 1x, https://evil.test/hi.png 2x">`, flagged: "https://evil.test/hi.png"},
		{name: "base href", markup: `<base href="https://evil.test/">`, flagged: "https://evil.test/"},
		{name: "inline style url", markup: `<div style="background:url(https://evil.test/bg.png)"></div>`, flagged: "https://evil.test/bg.png"},

		{name: "illustrative link", markup: `<a href="https://docs.example.test/guide">guide</a>`},
		{name: "json-ld context", markup: `<script type="application/ld+json">{"@context":"https://schema.org"}</script>`},
		{name: "open graph metadata", markup: `<meta property="og:image" content="https://cdn.example.test/hero.jpg">`},
		{name: "local srcset", markup: `<img srcset="a.png 1x, b.png 2x">`},
		{name: "canonical link", markup: `<link rel="canonical" href="https://example.test/page">`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "page.html")
			page := "<!doctype html><html><head></head><body>" + testCase.markup + "</body></html>"
			if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
				t.Fatal(err)
			}
			refs, err := ExternalResourceRefs(path)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.flagged == "" {
				if len(refs) != 0 {
					t.Fatalf("audit flagged %v in %s, which the browser does not fetch off this machine", refs, testCase.markup)
				}
				return
			}
			if !slices.Contains(refs, testCase.flagged) {
				t.Fatalf("audit found %v in %s, want %s among them", refs, testCase.markup, testCase.flagged)
			}
		})
	}
}

func TestAuditFixturesPassesASelfContainedPage(t *testing.T) {
	page := `<!doctype html><html><head><link rel="stylesheet" href="style.css"></head>
<body><img src="/img/logo.png"><script src="app.js"></script>
<a href="https://example.test/elsewhere">link</a></body></html>`
	path := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AuditFixtures([]string{path}); err != nil {
		t.Fatalf("a self-contained page was rejected: %v", err)
	}
}
