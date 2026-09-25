package urlread

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestFetchDiscoversSurfacesFromLLMsTxtAndLinks(t *testing.T) {
	const llmsTxt = "# Acme\n\n> Tools for agents.\n\n" +
		"- [Remote MCP server](https://mcp.acme.test/connect)\n" +
		"- [API reference](/api/openapi.yaml)\n" +
		"- [Swagger](https://acme.test/v2/swagger.json)\n" +
		"- [Stream](/mcp/sse)\n" +
		"- [Plugin](/.well-known/ai-plugin.json)\n" +
		"- [Agent](/.well-known/agent-card.json)\n" +
		"- [Docs](/docs)\n" +
		"Also see https://acme.test/mcp.\n"
	article := fixtureRoute{contentType: "text/html", body: "<html><body>" + articleBody + "</body></html>"}
	tests := []struct {
		name   string
		routes map[string]fixtureRoute
		want   func(base string) *AgentSurfaces
	}{
		{
			name: "llms.txt links to MCP, OpenAPI, a retired plugin manifest and an A2A card",
			routes: map[string]fixtureRoute{
				"/page":     article,
				"/llms.txt": {contentType: "text/markdown", body: llmsTxt},
			},
			want: func(base string) *AgentSurfaces {
				return &AgentSurfaces{
					APIDescriptions:    []string{base + "/api/openapi.yaml", "https://acme.test/v2/swagger.json"},
					MCP:                []string{"https://mcp.acme.test/connect", base + "/mcp/sse", "https://acme.test/mcp"},
					A2AAgentCard:       base + "/.well-known/agent-card.json",
					DeprecatedAIPlugin: base + "/.well-known/ai-plugin.json",
					LLMsTxt:            "present",
					LLMsTxtURL:         base + "/llms.txt",
				}
			},
		},
		{
			name: "an article about MCP and human API docs are not endpoints",
			routes: map[string]fixtureRoute{
				"/page": article,
				"/llms.txt": {contentType: "text/markdown", body: "# Shop\n\n> Book a call.\n\n" +
					"- [MCP Server Development](/services/mcp-development.md)\n" +
					"- [Booking over MCP](/mcp)\n"},
				"/.well-known/api-catalog": {contentType: "application/linkset+json", body: `{"linkset":[
					{"anchor":"/api","service-desc":[{"href":"/openapi.json"}],"service-doc":[{"href":"/llms-full.txt"},{"href":"/book.md"}]},
					{"anchor":"/.well-known/api-catalog","item":[{"href":"/api"}]}]}`},
			},
			want: func(base string) *AgentSurfaces {
				return &AgentSurfaces{
					APIDescriptions: []string{base + "/openapi.json"},
					APICatalog:      base + "/.well-known/api-catalog",
					MCP:             []string{base + "/mcp"},
					LLMsTxt:         "present",
					LLMsTxtURL:      base + "/llms.txt",
				}
			},
		},
		{
			name: "rel=mcp links, the Link header and well-known anchors",
			routes: map[string]fixtureRoute{
				"/page": {
					contentType: "text/html",
					header:      map[string]string{"Link": `</.well-known/api-catalog>; rel="api-catalog", <https://api.acme.test/openapi.json>; rel="service-desc"; type="application/openapi+json", </mcp>; rel=mcp`},
					body: `<html><head><link rel="mcp" href="https://mcp.acme.test/"><link rel="alternate" type="application/mcp+json" href="/server.json"></head><body>` +
						articleBody + `<a href="/.well-known/agent-card.json">agent</a><a href="/mcp">not a declaration</a></body></html>`,
				},
			},
			want: func(base string) *AgentSurfaces {
				return &AgentSurfaces{
					APIDescriptions: []string{"https://api.acme.test/openapi.json"},
					APICatalog:      base + "/.well-known/api-catalog",
					MCP:             []string{"https://mcp.acme.test/", base + "/server.json", base + "/mcp"},
					A2AAgentCard:    base + "/.well-known/agent-card.json",
					LLMsTxt:         "absent",
					LLMsTxtURL:      base + "/llms.txt",
				}
			},
		},
		{
			name: "well-known api-catalog, ai-catalog and ucp",
			routes: map[string]fixtureRoute{
				"/page": article,
				"/.well-known/api-catalog": {contentType: "application/linkset+json", body: `{"linkset":[
					{"anchor":"https://api.acme.test/v1","service-desc":[{"href":"https://api.acme.test/v1/openapi.json","type":"application/openapi+json"}],"service-doc":[{"href":"/docs/api"}]},
					{"anchor":"https://acme.test","item":[{"href":"/v2/openapi.json"},{"href":"/.well-known/ai-plugin.json"}]}]}`},
				"/.well-known/ai-catalog.json": {contentType: "application/json", body: `{"agents":[
					{"name":"shop","protocol":"MCP","url":"https://mcp.acme.test/shop"},
					{"name":"cards","mcp_servers":[{"endpoint":"https://mcp.acme.test/cards/server-card"}]},
					{"name":"a2a","protocol":"a2a","url":"https://acme.test/a2a"}]}`},
				"/.well-known/ucp": {contentType: "application/json", body: `{"version":"2026-01"}`},
			},
			want: func(base string) *AgentSurfaces {
				return &AgentSurfaces{
					APIDescriptions:    []string{"https://api.acme.test/v1/openapi.json", base + "/v2/openapi.json"},
					APICatalog:         base + "/.well-known/api-catalog",
					MCP:                []string{"https://mcp.acme.test/shop", "https://mcp.acme.test/cards"},
					UCP:                base + "/.well-known/ucp",
					DeprecatedAIPlugin: base + "/.well-known/ai-plugin.json",
					LLMsTxt:            "absent",
					LLMsTxtURL:         base + "/llms.txt",
				}
			},
		},
		{
			name: "SPA answering every probe with index.html reports nothing",
			routes: map[string]fixtureRoute{
				"/page":                        article,
				"/llms.txt":                    {contentType: "text/html", body: "<!doctype html><html><body><div id=root></div></body></html>"},
				"/page.md":                     {contentType: "text/plain", body: "<!doctype html><html><body></body></html>"},
				"/.well-known/api-catalog":     {contentType: "text/html", body: "<!doctype html><html></html>"},
				"/.well-known/ai-catalog.json": {contentType: "application/json", body: "<html><head></head></html>"},
				"/.well-known/ucp":             {contentType: "text/html", body: "<!doctype html><html></html>"},
			},
			want: func(base string) *AgentSurfaces {
				return &AgentSurfaces{LLMsTxt: "absent", LLMsTxtURL: base + "/llms.txt"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fixtureServer(t, tt.routes)
			result, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if want := tt.want(srv.URL); !reflect.DeepEqual(result.AgentSurfaces, want) {
				t.Fatalf("agent_surfaces =\n%+v\nwant\n%+v", result.AgentSurfaces, want)
			}
		})
	}
}

func TestFetchProbesRefusedByPolicyReportNothing(t *testing.T) {
	srv := fixtureServer(t, map[string]fixtureRoute{
		"/page":                        {contentType: "text/html", body: "<html><body>" + articleBody + "</body></html>"},
		"/.well-known/ucp":             {contentType: "application/json", body: `{}`},
		"/.well-known/ai-catalog.json": {contentType: "application/json", body: `{"protocol":"mcp","url":"https://m.test/mcp"}`},
	})
	result, err := Fetch(context.Background(), Options{
		URL: srv.URL + "/page",
		PolicyCheck: func(raw string) error {
			if strings.Contains(raw, "/.well-known/") {
				return errors.New("blocked")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if s := result.AgentSurfaces; s == nil || s.UCP != "" || len(s.MCP) != 0 {
		t.Fatalf("agent_surfaces = %+v, want refused probes to report nothing", s)
	}
}

func TestMarkdownVariantURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://a.test/docs/", "https://a.test/docs/index.html.md"},
		{"https://a.test", "https://a.test/index.html.md"},
		{"https://a.test/guide.html", "https://a.test/guide.html.md"},
		{"https://a.test/guide?x=1#top", "https://a.test/guide.md"},
		{"https://a.test/readme.md", ""},
		{"https://a.test/notes.TXT", ""},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := markdownVariantURL(u); got != tt.want {
			t.Errorf("markdownVariantURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFetchProbesMarkdownVariant(t *testing.T) {
	htmlPage := fixtureRoute{contentType: "text/html", body: "<html><body>" + articleBody + "</body></html>"}
	markdown := &fixtureRoute{contentType: "text/markdown", body: "# Page\n"}
	tests := []struct {
		name    string
		page    fixtureRoute
		variant *fixtureRoute
		policy  func(string) error
		want    bool
	}{
		{name: "present as markdown", page: htmlPage, variant: markdown, want: true},
		{name: "present with no content type", page: htmlPage, variant: &fixtureRoute{body: "# Page\n"}, want: true},
		{name: "absent", page: htmlPage},
		{name: "HTML 200 from an SPA is absent", page: htmlPage, variant: &fixtureRoute{contentType: "text/html", body: "<!doctype html><html><body></body></html>"}},
		{
			name: "policy refusal", page: htmlPage, variant: markdown,
			policy: func(raw string) error {
				if strings.HasSuffix(raw, ".md") {
					return errors.New("blocked")
				}
				return nil
			},
		},
		{name: "not reported when the page was already markdown", page: fixtureRoute{contentType: "text/markdown", body: "# Page\n\nprose\n"}, variant: markdown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := map[string]fixtureRoute{"/guide": tt.page}
			if tt.variant != nil {
				routes["/guide.md"] = *tt.variant
			}
			srv := fixtureServer(t, routes)
			result, err := Fetch(context.Background(), Options{URL: srv.URL + "/guide", PolicyCheck: tt.policy})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			var got []string
			if result.AgentSurfaces != nil {
				got = result.AgentSurfaces.Markdown
			}
			if want := []string{srv.URL + "/guide.md"}; tt.want != reflect.DeepEqual(got, want) {
				t.Fatalf("markdown = %v, want reported=%v", got, tt.want)
			}
		})
	}
}

func TestParseLinkHeader(t *testing.T) {
	got := parseLinkHeader([]string{`<https://a.test/x>; rel="service-desc api-catalog"; type="application/openapi+json; v=3", <b>;rel=mcp`, `</c>`})
	want := []surfaceLink{
		{href: "https://a.test/x", rels: []string{"service-desc", "api-catalog"}, typ: "application/openapi+json"},
		{href: "b", rels: []string{"mcp"}},
		{href: "/c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLinkHeader = %+v, want %+v", got, want)
	}
}

func TestFetchReportsContentSignalAndMarkdownTokens(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name       string
		header     map[string]string
		wantSignal string
		wantTokens int
	}{
		{name: "both", header: map[string]string{"Content-Signal": " search=yes, ai-train=no ", "X-Markdown-Tokens": "1234"}, wantSignal: "search=yes, ai-train=no", wantTokens: 1234},
		{name: "unparseable tokens", header: map[string]string{"X-Markdown-Tokens": "lots"}},
		{name: "signal is capped", header: map[string]string{"Content-Signal": long}, wantSignal: long[:200]},
		{name: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fixtureServer(t, map[string]fixtureRoute{"/p": {contentType: "text/markdown", header: tt.header, body: "# P\n"}})
			result, err := Fetch(context.Background(), Options{URL: srv.URL + "/p"})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if result.ContentSignal != tt.wantSignal || result.MarkdownTokens != tt.wantTokens {
				t.Fatalf("content_signal=%q markdown_tokens=%d, want %q %d", result.ContentSignal, result.MarkdownTokens, tt.wantSignal, tt.wantTokens)
			}
		})
	}
}

func TestUserAgentFor(t *testing.T) {
	tests := []struct{ version, want string }{
		{"", "brw/dev (+https://brw.donworks.co.uk)"},
		{" 0.14.0 ", "brw/0.14.0 (+https://brw.donworks.co.uk)"},
	}
	for _, tt := range tests {
		if got := UserAgentFor(tt.version); got != tt.want {
			t.Errorf("UserAgentFor(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestFetchSendsOneHonestUserAgentEverywhere(t *testing.T) {
	var mu sync.Mutex
	agents := map[string]string{}
	var pageAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents[r.URL.Path] = r.Header.Get("User-Agent")
		if r.URL.Path == "/page" {
			pageAccept = r.Header.Get("Accept")
		}
		mu.Unlock()
		if r.URL.Path != "/page" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>"+articleBody+"</body></html>")
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if pageAccept != "text/markdown, text/plain;q=0.95, text/html;q=0.9, application/xhtml+xml;q=0.9" {
		t.Errorf("Accept = %q", pageAccept)
	}
	for _, path := range []string{"/page", "/llms.txt", "/page.md", "/.well-known/api-catalog", "/.well-known/ai-catalog.json", "/.well-known/ucp"} {
		if got := agents[path]; got != UserAgentFor("") {
			t.Errorf("%s User-Agent = %q, want %q", path, got, UserAgentFor(""))
		}
	}
}

// BenchmarkFetchDiscoveryOverhead measures what the probe batch adds to a read
// against a loopback server where every probe answers 404.
func BenchmarkFetchDiscoveryOverhead(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/page" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>"+articleBody+"</body></html>")
	}))
	defer srv.Close()
	for _, skip := range []bool{true, false} {
		name := "with_probes"
		if skip {
			name = "without_probes"
		}
		b.Run(name, func(b *testing.B) {
			skipDiscovery = skip
			defer func() { skipDiscovery = false }()
			for i := 0; i < b.N; i++ {
				if _, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
