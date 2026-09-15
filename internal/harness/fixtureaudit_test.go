package harness

import (
	"os"
	"path/filepath"
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
