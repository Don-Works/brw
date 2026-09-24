package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
)

func TestInlineDocumentHeadersRewritesOnlyDownloadShapedText(t *testing.T) {
	cases := []struct {
		name        string
		in          []*fetch.HeaderEntry
		wantChanged bool
		wantBody    bool
		wantType    string
		wantDispo   string
	}{
		{
			name:        "attachment disposition is dropped",
			in:          []*fetch.HeaderEntry{{Name: "Content-Type", Value: "text/javascript; charset=UTF-8"}, {Name: "Content-Disposition", Value: `attachment; filename="f.txt"`}},
			wantChanged: true,
			wantType:    "text/javascript; charset=UTF-8",
		},
		{
			name:        "inline disposition is kept",
			in:          []*fetch.HeaderEntry{{Name: "content-disposition", Value: "inline"}},
			wantChanged: false,
			wantDispo:   "inline",
		},
		{
			name:        "csv becomes text/plain with its charset and needs the body re-served",
			in:          []*fetch.HeaderEntry{{Name: "content-type", Value: "text/csv; charset=utf-8"}, {Name: "Content-Length", Value: "18"}, {Name: "Content-Encoding", Value: "gzip"}},
			wantChanged: true,
			wantBody:    true,
			wantType:    "text/plain; charset=utf-8",
		},
		{
			name:        "ndjson becomes text/plain",
			in:          []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/x-ndjson"}},
			wantChanged: true,
			wantBody:    true,
			wantType:    "text/plain",
		},
		{
			name:        "json renders already and is untouched",
			in:          []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/json"}},
			wantChanged: false,
			wantType:    "application/json",
		},
		{
			name:        "xml renders already and is untouched",
			in:          []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/xml"}},
			wantChanged: false,
			wantType:    "application/xml",
		},
		{
			name:        "binary stays a download",
			in:          []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/octet-stream"}},
			wantChanged: false,
			wantType:    "application/octet-stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewrite := InlineDocumentHeaders(tc.in)
			got := rewrite.Headers
			if rewrite.Changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (headers %v)", rewrite.Changed, tc.wantChanged, renderHeaders(got))
			}
			if rewrite.NeedsBody != tc.wantBody {
				t.Fatalf("needsBody = %v, want %v", rewrite.NeedsBody, tc.wantBody)
			}
			if rewrite.NeedsBody && (fetchHeaderValue(got, "content-length") != "" || fetchHeaderValue(got, "content-encoding") != "") {
				t.Fatalf("a re-served body kept its wire-form headers: %v", renderHeaders(got))
			}
			if typ := fetchHeaderValue(got, "content-type"); typ != tc.wantType {
				t.Fatalf("content-type = %q, want %q", typ, tc.wantType)
			}
			if dispo := fetchHeaderValue(got, "content-disposition"); dispo != tc.wantDispo {
				t.Fatalf("content-disposition = %q, want %q", dispo, tc.wantDispo)
			}
		})
	}
}

func TestInlineDocumentBodyWithinLimit(t *testing.T) {
	if !InlineDocumentBodyWithinLimit(nil) {
		t.Fatal("an undeclared length was refused")
	}
	if !InlineDocumentBodyWithinLimit([]*fetch.HeaderEntry{{Name: "Content-Length", Value: "1024"}}) {
		t.Fatal("a small body was refused")
	}
	if InlineDocumentBodyWithinLimit([]*fetch.HeaderEntry{{Name: "content-length", Value: "99999999"}}) {
		t.Fatal("a body over the limit was accepted")
	}
}

func fetchHeaderValue(headers []*fetch.HeaderEntry, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

func renderHeaders(headers []*fetch.HeaderEntry) []string {
	out := make([]string, 0, len(headers))
	for _, header := range headers {
		out = append(out, header.Name+": "+header.Value)
	}
	return out
}

// rawDocumentServer serves the response shapes that used to turn a navigation
// into a download: a JSON body flagged as an attachment (what Google's suggest
// endpoint sends), an XML sitemap flagged the same way, a CSV with no
// disposition at all, and a binary body that must remain a download.
func rawDocumentServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	serve := func(path, contentType, disposition, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			if disposition != "" {
				w.Header().Set("Content-Disposition", disposition)
			}
			fmt.Fprint(w, body)
		})
	}
	serve("/suggest.json", "text/javascript; charset=UTF-8", `attachment; filename="f.txt"`, `["wine",["wine glasses","wine rack"]]`)
	serve("/data.json", "application/json", `attachment; filename="data.json"`, `{"current_user_url":"https://api.example/user","count":3}`)
	serve("/sitemap.xml", "application/xml", `attachment; filename="sitemap.xml"`, `<?xml version="1.0"?><urlset><url><loc>https://example.test/page-one</loc></url></urlset>`)
	serve("/rows.csv", "text/csv", "", "sku,price\nA1,9.99\n")
	serve("/blob.bin", "application/octet-stream", `attachment; filename="blob.bin"`, "\x00\x01\x02binary")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNavigateToRendersDownloadShapedDocumentsInline(t *testing.T) {
	m := newHeadlessManager(t)
	srv := rawDocumentServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := m.Open(ctx, "about:blank"); err != nil {
		t.Fatalf("open: %v", err)
	}

	cases := []struct {
		path string
		want string
	}{
		{"/suggest.json", `"wine glasses"`},
		{"/data.json", `"count":3`},
		{"/sitemap.xml", "https://example.test/page-one"},
		{"/rows.csv", "A1,9.99"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			target := srv.URL + tc.path
			result, err := m.NavigateTo(ctx, target)
			if err != nil {
				t.Fatalf("navigate_to %s: %v", tc.path, err)
			}
			if result.URL != target {
				t.Fatalf("navigate_to landed on %q, want %q", result.URL, target)
			}
			read, err := m.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if read.URL != target {
				t.Fatalf("read reports %q, want the navigated document %q", read.URL, target)
			}
			if !strings.Contains(read.Main, tc.want) {
				t.Fatalf("read main = %q, want it to carry %q", read.Main, tc.want)
			}
		})
	}

	t.Run("read_data parses the JSON body", func(t *testing.T) {
		if _, err := m.NavigateTo(ctx, srv.URL+"/data.json"); err != nil {
			t.Fatalf("navigate_to: %v", err)
		}
		data, err := m.ReadData(ctx)
		if err != nil {
			t.Fatalf("read_data: %v", err)
		}
		if data.Source != "json_document" {
			t.Fatalf("read_data source = %q, want json_document (%+v)", data.Source, data)
		}
		raw, _ := data.Raw.(map[string]any)
		if count, _ := raw["count"].(float64); count != 3 {
			t.Fatalf("read_data raw = %#v, want the parsed body", data.Raw)
		}
	})

	t.Run("binary attachment is still refused by name", func(t *testing.T) {
		_, err := m.NavigateTo(ctx, srv.URL+"/blob.bin")
		if err == nil {
			t.Fatal("navigate_to a binary attachment succeeded")
		}
		if !strings.Contains(err.Error(), "served as a download") {
			t.Fatalf("navigate_to error = %q, want it to say the destination was a download", err)
		}
	})

	t.Run("interception is released after the navigation", func(t *testing.T) {
		tabID, err := m.ensureActive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m.containment.inlineDocumentArmed(tabID) {
			t.Fatal("inline-document arm still set after navigate_to returned")
		}
		if enable, _, _ := m.fetchInterceptionCommand(tabID); enable {
			t.Fatal("Fetch interception still requested with nothing left to intercept for")
		}
	})
}

func TestOpenRendersDownloadShapedDocumentInline(t *testing.T) {
	m := newHeadlessManager(t)
	srv := rawDocumentServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := srv.URL + "/suggest.json"
	result, err := m.Open(ctx, target)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !result.Ready {
		t.Fatal("open reported ready=false for a text document brw renders inline")
	}
	if result.Tab.URL != target {
		t.Fatalf("open landed on %q, want %q", result.Tab.URL, target)
	}
	read, err := m.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(read.Main, `"wine rack"`) {
		t.Fatalf("read main = %q, want the JSON body", read.Main)
	}
}

func TestInlineDocumentPatternCoversOnlyTheDestinationOrigin(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://www.google.com/complete/search?q=wine#frag", "https://www.google.com/*"},
		{"http://127.0.0.1:54882/data.json", "http://127.0.0.1:54882/*"},
		{"HTTP://Example.COM:80/a", "http://example.com/*"},
		{"https://example.com:443/", "https://example.com/*"},
		{"https://example.com:8443/", "https://example.com:8443/*"},
		{"http://[::1]:9000/x", "http://[::1]:9000/*"},
		{"about:blank", ""},
		{"file:///tmp/x.json", ""},
		{"data:text/plain,hi", ""},
		{"::not a url", ""},
	}
	for _, tc := range cases {
		if got := inlineDocumentPattern(tc.url); got != tc.want {
			t.Errorf("inlineDocumentPattern(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}

	m := &Manager{}
	m.containment.addInlineDocumentPattern("tab", inlineDocumentPattern("http://127.0.0.1:1/x"))
	_, _, patterns := m.fetchInterceptionCommand("tab")
	if len(patterns) != 1 || patterns[0].URLPattern != "http://127.0.0.1:1/*" || patterns[0].RequestStage != fetch.RequestStageResponse {
		t.Fatalf("patterns = %+v, want one response-stage pattern for the destination origin", patterns)
	}
}

func TestNavigateToFollowsACrossOriginRedirectToAnInlineDocument(t *testing.T) {
	m := newHeadlessManager(t)
	docs := rawDocumentServer(t)
	// localhost and 127.0.0.1 are different origins, so the redirect leaves the
	// origin the navigation was armed for.
	target := strings.Replace(docs.URL, "127.0.0.1", "localhost", 1) + "/suggest.json"
	redirector := httptest.NewServer(http.RedirectHandler(target, http.StatusFound))
	t.Cleanup(redirector.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := m.Open(ctx, "about:blank"); err != nil {
		t.Fatalf("open: %v", err)
	}

	result, err := m.NavigateTo(ctx, redirector.URL+"/go")
	if err != nil {
		t.Fatalf("navigate_to through a cross-origin redirect: %v", err)
	}
	if result.URL != target {
		t.Fatalf("navigate_to landed on %q, want %q", result.URL, target)
	}
	read, err := m.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(read.Main, `"wine rack"`) {
		t.Fatalf("read main = %q, want the JSON body rendered inline", read.Main)
	}
}
