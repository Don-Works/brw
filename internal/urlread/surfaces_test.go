package urlread

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureRoute struct {
	status      int
	contentType string
	header      map[string]string
	body        string
	delay       time.Duration
}

func fixtureServer(t *testing.T, routes map[string]fixtureRoute) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if route.delay > 0 {
			select {
			case <-time.After(route.delay):
			case <-r.Context().Done():
				return
			}
		}
		for k, v := range route.header {
			w.Header().Set(k, v)
		}
		if route.contentType != "" {
			w.Header().Set("Content-Type", route.contentType)
		}
		status := route.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		fmt.Fprint(w, route.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const articleBody = `<main><h1>Article</h1><p>` +
	`This page has plenty of real prose so it is not mistaken for an empty shell. ` +
	`It keeps going for a while, describing the product, the pricing, the support ` +
	`options and everything else a reader would come here for in the first place.</p></main>`

func TestFetchReportsDeclaredAgentSurfaces(t *testing.T) {
	head := `<html><head><title>Docs</title>
		<link rel="alternate" type="text/markdown" href="/page.md">
		<link rel="alternate" type="text/markdown" href="/page.md">
		<link rel="Alternate stylesheet" type="text/css" href="/x.css">
		<link rel="service-desc" href="/openapi.json">
		<link rel="alternate" type="application/vnd.oai.openapi+json;version=3.1" href="https://api.example.test/spec">
		<link rel="llms" href="/llms-full.txt">
		<link rel="alternate" type="text/markdown" href="javascript:alert(1)">
		</head><body>` + articleBody + `</body></html>`
	srv := fixtureServer(t, map[string]fixtureRoute{
		"/page": {contentType: "text/html", body: head},
	})
	result, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := result.AgentSurfaces
	if got == nil {
		t.Fatal("agent_surfaces missing")
	}
	want := &AgentSurfaces{
		Markdown:        []string{srv.URL + "/page.md"},
		LLMs:            []string{srv.URL + "/llms-full.txt"},
		APIDescriptions: []string{srv.URL + "/openapi.json", "https://api.example.test/spec"},
		LLMsTxt:         "absent",
		LLMsTxtURL:      srv.URL + "/llms.txt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent_surfaces = %+v, want %+v", got, want)
	}
	if result.FallbackHint != "" {
		t.Fatalf("fallback_hint = %q, want none", result.FallbackHint)
	}
}

func TestFetchProbesLLMsTxt(t *testing.T) {
	page := fixtureRoute{contentType: "text/html", body: "<html><body>" + articleBody + "</body></html>"}
	tests := []struct {
		name   string
		llms   *fixtureRoute
		policy func(string) error
		want   string
	}{
		{name: "present as plain text", llms: &fixtureRoute{contentType: "text/plain", body: "# Site\n\n- [Docs](/docs)\n"}, want: "present"},
		{name: "present as markdown", llms: &fixtureRoute{contentType: "text/markdown; charset=utf-8", body: "# Site\n"}, want: "present"},
		{name: "404 is absent", want: "absent"},
		{name: "SPA answering index.html with 200 is absent", llms: &fixtureRoute{contentType: "text/html", body: "<!doctype html><html><body><div id=root></div></body></html>"}, want: "absent"},
		{name: "html body served as text/plain is absent", llms: &fixtureRoute{contentType: "text/plain", body: "<!DOCTYPE html><html><head></head></html>"}, want: "absent"},
		{name: "slow probe is unknown", llms: &fixtureRoute{contentType: "text/plain", body: "# late", delay: 2 * time.Second}, want: "unknown"},
		{
			name: "policy refusing the probe is unknown and the read still succeeds",
			llms: &fixtureRoute{contentType: "text/plain", body: "# Site"},
			policy: func(raw string) error {
				if strings.HasSuffix(raw, "/llms.txt") {
					return errors.New("blocked")
				}
				return nil
			},
			want: "unknown",
		},
	}
	restore := llmsProbeTimeout
	llmsProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { llmsProbeTimeout = restore })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := map[string]fixtureRoute{"/page": page}
			if tt.llms != nil {
				routes["/llms.txt"] = *tt.llms
			}
			srv := fixtureServer(t, routes)
			start := time.Now()
			result, err := Fetch(context.Background(), Options{URL: srv.URL + "/page", PolicyCheck: tt.policy})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if elapsed := time.Since(start); elapsed > llmsProbeTimeout+time.Second {
				t.Fatalf("read took %v, longer than the probe budget", elapsed)
			}
			if result.AgentSurfaces == nil || result.AgentSurfaces.LLMsTxt != tt.want {
				t.Fatalf("agent_surfaces = %+v, want llms_txt %q", result.AgentSurfaces, tt.want)
			}
		})
	}
}

func TestFetchSkipsProbeWhenReadingLLMsTxt(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/llms.txt" {
			probes.Add(1)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "# Site\n")
	}))
	defer srv.Close()
	for _, opts := range []Options{{URL: srv.URL, LLMs: true}, {URL: srv.URL + "/llms.txt"}} {
		result, err := Fetch(context.Background(), opts)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if result.AgentSurfaces != nil {
			t.Fatalf("agent_surfaces = %+v, want none", result.AgentSurfaces)
		}
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("/llms.txt requested %d times, want exactly the 2 direct reads", n)
	}
}

func TestFetchFallbackHints(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		route   fixtureRoute
		want    string
		wantErr string
	}{
		{
			name:  "ordinary article has no hint",
			path:  "/article",
			route: fixtureRoute{contentType: "text/html", body: "<html><head><title>News</title><script src=/app.js></script></head><body>" + articleBody + "</body></html>"},
		},
		{
			name: "login page by path",
			path: "/account/login",
			route: fixtureRoute{contentType: "text/html", body: `<html><head><title>Acme</title></head><body>
				<form><input name=email><input type="password" name=pw><button>Go</button></form></body></html>`},
			want: HintLoginWall,
		},
		{
			name: "login page by title",
			path: "/dashboard",
			route: fixtureRoute{contentType: "text/html", body: `<html><head><title>Sign in to Acme</title></head><body>
				<form><input type=PASSWORD></form></body></html>`},
			want: HintLoginWall,
		},
		{
			name: "password field on an ordinary page is not a login wall",
			path: "/settings",
			route: fixtureRoute{contentType: "text/html", body: `<html><head><title>Settings</title></head><body>` + articleBody +
				`<form><input type=password></form></body></html>`},
		},
		{
			name: "empty react shell",
			path: "/app",
			route: fixtureRoute{contentType: "text/html", body: `<!doctype html><html><head><title>App</title>
				<script type=module src="/assets/index-abc.js"></script></head><body><div id="root"></div></body></html>`},
			want: HintJSShell,
		},
		{
			name: "noscript shell",
			path: "/spa",
			route: fixtureRoute{contentType: "text/html", body: `<html><head><title>X</title></head><body>
				<noscript>You need to enable JavaScript to run this app.</noscript><div id="main"></div>
				<script>window.boot()</script></body></html>`},
			want: HintJSShell,
		},
		{
			name: "server-rendered next page is not a shell",
			path: "/ssr",
			route: fixtureRoute{contentType: "text/html", body: `<html><body><div id="__next">` + articleBody +
				`</div><script src=/_next/main.js></script></body></html>`},
		},
		{
			name:  "cloudflare challenge on 200",
			path:  "/guarded",
			route: fixtureRoute{contentType: "text/html", body: `<html><head><title>Just a moment...</title></head><body><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/jsch/v1"></script></body></html>`},
			want:  HintChallenge,
		},
		{
			name:    "cloudflare challenge on 403",
			path:    "/guarded",
			route:   fixtureRoute{status: http.StatusForbidden, contentType: "text/html", header: map[string]string{"cf-mitigated": "challenge"}, body: "<html><head><title>Attention Required! | Cloudflare</title></head></html>"},
			want:    HintChallenge,
			wantErr: "HTTP 403 (challenge",
		},
		{
			name:    "401 needs auth",
			path:    "/private",
			route:   fixtureRoute{status: http.StatusUnauthorized, contentType: "text/plain", body: "unauthorized"},
			want:    HintAuthRequired,
			wantErr: "HTTP 401 (auth_required",
		},
		{
			name:    "plain 403 needs auth",
			path:    "/private",
			route:   fixtureRoute{status: http.StatusForbidden, contentType: "text/html", body: "<html><title>Forbidden</title></html>"},
			want:    HintAuthRequired,
			wantErr: "HTTP 403 (auth_required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fixtureServer(t, map[string]fixtureRoute{tt.path: tt.route})
			result, err := Fetch(context.Background(), Options{URL: srv.URL + tt.path})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if result.FallbackHint != tt.want {
				t.Fatalf("fallback_hint = %q, want %q", result.FallbackHint, tt.want)
			}
		})
	}
}
