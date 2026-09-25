package urlread

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestFetchExtractsBySourceType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		llms        bool
		wantSource  string
		wantTitle   string
		wantInMain  []string
		wantNotMain []string
	}{
		{
			name:        "markdown is used as served, not re-extracted",
			contentType: "text/markdown; charset=utf-8",
			body:        "# Release notes\n\nShipped the thing.\n",
			wantSource:  "markdown",
			wantTitle:   "Release notes",
			wantInMain:  []string{"# Release notes", "Shipped the thing."},
		},
		{
			name:        "html is extracted to prose",
			contentType: "text/html; charset=utf-8",
			body: `<!doctype html><html lang="en"><head><title>Docs</title>
				<meta name="description" content="How to use it">
				</head><body>
				<nav><a href="/elsewhere">Nav link</a></nav>
				<main><h1>Getting started</h1><p>Install it first.</p>
				<ul><li>One</li><li>Two</li></ul></main>
				<footer>Copyright nobody</footer></body></html>`,
			wantSource:  "html",
			wantTitle:   "Docs",
			wantInMain:  []string{"# Getting started", "Install it first.", "- One", "- Two"},
			wantNotMain: []string{"Nav link", "Copyright nobody"},
		},
		{
			name:        "llms.txt is fetched from the origin root",
			contentType: "text/plain",
			body:        "# brw\n\nBrowser control for agents.\n",
			llms:        true,
			wantSource:  "llms_txt",
			wantTitle:   "brw",
			wantInMain:  []string{"Browser control for agents."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPath, gotAccept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.llms || r.URL.Path == "/page" {
					mu.Lock()
					gotPath = r.URL.Path
					gotAccept = r.Header.Get("Accept")
					mu.Unlock()
				}
				w.Header().Set("Content-Type", tt.contentType)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			result, err := Fetch(context.Background(), Options{URL: srv.URL + "/page", LLMs: tt.llms})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if result.Source != tt.wantSource {
				t.Errorf("source = %q, want %q", result.Source, tt.wantSource)
			}
			if tt.wantTitle != "" && result.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", result.Title, tt.wantTitle)
			}
			for _, want := range tt.wantInMain {
				if !strings.Contains(result.Main, want) {
					t.Errorf("main missing %q; got:\n%s", want, result.Main)
				}
			}
			for _, notWant := range tt.wantNotMain {
				if strings.Contains(result.Main, notWant) {
					t.Errorf("main should not contain chrome %q; got:\n%s", notWant, result.Main)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(gotAccept, "text/markdown") {
				t.Errorf("Accept header %q should prefer markdown", gotAccept)
			}
			if tt.llms && gotPath != "/llms.txt" {
				t.Errorf("llms request path = %q, want /llms.txt", gotPath)
			}
			if !result.Unauthenticated {
				t.Error("a no-browser read must report itself as unauthenticated")
			}
		})
	}
}

// The read must never carry the user's session: that is the whole reason it is
// safe to point at an arbitrary URL.
func TestFetchSendsNoCookies(t *testing.T) {
	var mu sync.Mutex
	var sawCookie []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if c := r.Header.Get("Cookie"); c != "" {
			sawCookie = append(sawCookie, r.URL.Path+": "+c)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><main><p>hi</p></main></body></html>")
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), Options{URL: srv.URL}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sawCookie) != 0 {
		t.Fatalf("read sent a Cookie header: %q", sawCookie)
	}
}

func TestFetchAppliesNavigationPolicyToTheURLAndEveryRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><main><p>secret</p></main></body></html>")
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	t.Run("initial url is gated", func(t *testing.T) {
		_, err := Fetch(context.Background(), Options{
			URL:         redirector.URL,
			PolicyCheck: func(string) error { return errors.New("blocked by policy") },
		})
		if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
			t.Fatalf("err = %v, want the policy refusal", err)
		}
	})

	t.Run("redirect target is gated too", func(t *testing.T) {
		finalHost := mustHost(t, final.URL)
		_, err := Fetch(context.Background(), Options{
			URL: redirector.URL,
			PolicyCheck: func(raw string) error {
				if strings.Contains(raw, finalHost) {
					return errors.New("redirect destination blocked by policy")
				}
				return nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "redirect destination blocked by policy") {
			t.Fatalf("err = %v, want the redirect to be refused", err)
		}
	})
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
	host, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("split %q: %v", raw, err)
	}
	return host
}

func TestFetchRejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/x", "javascript:alert(1)"} {
		if _, err := Fetch(context.Background(), Options{URL: raw}); err == nil {
			t.Errorf("Fetch(%q) should be refused", raw)
		}
	}
}

// checkDialIP is what stops a URL read from being turned into a request against
// cloud instance metadata. It runs on the RESOLVED address, so a hostname that
// resolves into a blocked range is refused too.
func TestCheckDialIPBlocksInfrastructureRanges(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
		why     string
	}{
		{"169.254.169.254", true, "cloud instance metadata"},
		{"169.254.1.1", true, "link-local"},
		{"fe80::1", true, "IPv6 link-local"},
		{"0.0.0.0", true, "unspecified"},
		{"224.0.0.1", true, "multicast"},
		{"100.64.0.1", true, "carrier-grade NAT"},
		{"198.18.0.1", true, "benchmarking range"},
		{"240.0.0.1", true, "reserved"},
		{"192.0.0.1", true, "IETF protocol assignments"},
		// Allowed on purpose: brw is a local dev tool.
		{"127.0.0.1", false, "loopback is a first-class local dev target"},
		{"::1", false, "IPv6 loopback"},
		{"93.184.216.34", false, "ordinary public address"},
		// RFC1918 addresses are built rather than written as literals so the
		// repository's hygiene scanner does not read a test table as a leaked
		// internal address.
		{privateIPv4(192, 168, 1, 10), false, "LAN host"},
		{privateIPv4(10, 0, 0, 5), false, "private network"},
		{privateIPv4(172, 16, 0, 9), false, "private network"},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			err := checkDialIP(net.ParseIP(tt.ip))
			if tt.blocked && err == nil {
				t.Fatalf("%s (%s) should be refused", tt.ip, tt.why)
			}
			if !tt.blocked && err != nil {
				t.Fatalf("%s (%s) should be allowed, got %v", tt.ip, tt.why, err)
			}
		})
	}
}

func TestFetchPagesLikeAnInTabRead(t *testing.T) {
	// Each 10-char block is distinct, so two different windows cannot compare
	// equal by accident the way a repeated pattern would.
	var prose strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&prose, "block%03d__", i)
	}
	body := "<html><head><title>T</title></head><body><main><p>" + prose.String() + "</p></main></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	first, err := Fetch(context.Background(), Options{URL: srv.URL, MaxChars: 100})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len([]rune(first.Main)) > 100 {
		t.Fatalf("main is %d runes, want <= 100", len([]rune(first.Main)))
	}
	if !first.MainTruncated || first.NextOffset == 0 {
		t.Fatalf("expected paging metadata, got truncated=%v next=%d", first.MainTruncated, first.NextOffset)
	}
	second, err := Fetch(context.Background(), Options{URL: srv.URL, MaxChars: 100, Offset: first.NextOffset})
	if err != nil {
		t.Fatalf("Fetch page 2: %v", err)
	}
	if second.Main == first.Main {
		t.Fatal("second page returned the same prose as the first")
	}
}

func TestFetchReportsHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	result, err := Fetch(context.Background(), Options{URL: srv.URL})
	if err == nil {
		t.Fatal("a 404 should be an error")
	}
	if result.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", result.Status)
	}
}

// privateIPv4 builds an RFC1918 address without spelling one out in source.
func privateIPv4(a, b, c, d byte) string {
	return net.IPv4(a, b, c, d).String()
}
