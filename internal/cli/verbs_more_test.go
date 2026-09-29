package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moreResponses() map[string]string {
	responses := daemonResponses()
	for _, v := range append(append(append(pageVerbs(), inspectVerbs()...), envVerbs()...), tabVerbs()...) {
		responses[v.method+" "+v.path] = actionResponse
	}
	responses["GET /api/page/console"] = `[{"level":"error","text":"boom"}]`
	responses["POST /api/page/cookies"] = `{"action":"list","url":"https://example.test/","cookies":[{"name":"sid","value":"never-printed","domain":"example.test","path":"/"}],"count":1}`
	responses["POST /api/browser/open_incognito"] = openResponse
	responses["GET /api/visual/screenshot_element"] = screenshotResponse
	return responses
}

func TestPageInspectEnvAndTabVerbs(t *testing.T) {
	batchFile := filepath.Join(t.TempDir(), "steps.json")
	if err := os.WriteFile(batchFile, []byte("[{\"action\":\"press\",\"key\":\"Enter\"}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantBody   string
		wantStdout []string
		notStdout  []string
	}{
		{name: "goto", args: []string{"goto", "https://example.test/next"}, wantMethod: http.MethodPost, wantPath: "/api/page/navigate_to", wantBody: `{"url":"https://example.test/next"}`},
		{name: "back", args: []string{"back"}, wantMethod: http.MethodPost, wantPath: "/api/page/navigate", wantBody: `{"direction":"back"}`},
		{name: "reload", args: []string{"reload"}, wantMethod: http.MethodPost, wantPath: "/api/page/navigate", wantBody: `{"direction":"reload"}`},
		{name: "hover", args: []string{"hover", "@e17"}, wantMethod: http.MethodPost, wantPath: "/api/page/hover", wantBody: `{"ref":"e17"}`},
		{name: "dblclick", args: []string{"dblclick", "@e17"}, wantMethod: http.MethodPost, wantPath: "/api/page/click", wantBody: `{"click_count":2,"ref":"e17"}`},
		{name: "click with a count and button", args: []string{"click", "@e17", "--count", "3", "--button", "right"}, wantMethod: http.MethodPost, wantPath: "/api/page/click", wantBody: `{"button":"right","click_count":3,"ref":"e17"}`},
		{name: "select", args: []string{"select", "@e18", "uk"}, wantMethod: http.MethodPost, wantPath: "/api/page/select", wantBody: `{"ref":"e18","value":"uk"}`},
		{name: "scroll a direction", args: []string{"scroll", "Down"}, wantMethod: http.MethodPost, wantPath: "/api/page/scroll", wantBody: `{"direction":"down"}`},
		{name: "scroll a ref into view", args: []string{"scroll", "@e9"}, wantMethod: http.MethodPost, wantPath: "/api/page/scroll", wantBody: `{"ref":"e9"}`},
		{name: "upload", args: []string{"upload", "@e3", "/tmp/report.pdf"}, wantMethod: http.MethodPost, wantPath: "/api/page/upload_file", wantBody: `{"path":"/tmp/report.pdf","ref":"e3"}`},
		{name: "drag", args: []string{"drag", "@e1", "@e2"}, wantMethod: http.MethodPost, wantPath: "/api/page/drag", wantBody: `{"from":{"ref":"e1"},"to":{"ref":"e2"}}`},
		{name: "clickxy", args: []string{"clickxy", "10", "20.5"}, wantMethod: http.MethodPost, wantPath: "/api/page/click_xy", wantBody: `{"x":10,"y":20.5}`},
		{name: "keydown", args: []string{"keydown", "Shift"}, wantMethod: http.MethodPost, wantPath: "/api/page/key_down", wantBody: `{"key":"Shift"}`},
		{name: "commit", args: []string{"commit", "@e4"}, wantMethod: http.MethodPost, wantPath: "/api/page/commit", wantBody: `{"ref":"e4"}`},
		{name: "assert text forwards the timeout", args: []string{"assert", "text", "@e4", "Saved", "--timeout", "2s"}, wantMethod: http.MethodPost, wantPath: "/api/page/assert_text", wantBody: `{"ref":"e4","text":"Saved","timeout_ms":2000}`},
		{name: "assert hidden", args: []string{"assert", "hidden", "@e4"}, wantMethod: http.MethodPost, wantPath: "/api/page/assert_hidden", wantBody: `{"ref":"e4"}`},
		{name: "pushstate", args: []string{"pushstate", "/next"}, wantMethod: http.MethodPost, wantPath: "/api/page/pushstate", wantBody: `{"url":"/next"}`},
		{name: "frame main", args: []string{"frame", "MAIN"}, wantMethod: http.MethodPost, wantPath: "/api/page/frame", wantBody: `{"target":"main"}`},
		{name: "get", args: []string{"get", "attr", "@e17", "href"}, wantMethod: http.MethodGet, wantPath: "/api/page/get", wantQuery: url.Values{"what": {"attr"}, "target": {"e17"}, "name": {"href"}}},
		{name: "eval", args: []string{"eval", "1+1"}, wantMethod: http.MethodPost, wantPath: "/api/page/evaluate", wantBody: `{"expression":"1+1"}`},
		{name: "console", args: []string{"console", "--errors", "--limit", "5"}, wantMethod: http.MethodGet, wantPath: "/api/page/console", wantQuery: url.Values{"only_errors": {"true"}, "limit": {"5"}}, wantStdout: []string{"error", "boom"}},
		{name: "cookies list prints no values", args: []string{"cookies"}, wantMethod: http.MethodPost, wantPath: "/api/page/cookies", wantBody: `{"action":"list"}`, wantStdout: []string{"sid", "example.test"}, notStdout: []string{"never-printed"}},
		{name: "cookies list for a url", args: []string{"cookies", "list", "https://example.test/"}, wantMethod: http.MethodPost, wantPath: "/api/page/cookies", wantBody: `{"action":"list","url":"https://example.test/"}`},
		{name: "cookies set", args: []string{"cookies", "set", "sid", "v", "--domain", "example.test", "--secure", "--same-site", "lax"}, wantMethod: http.MethodPost, wantPath: "/api/page/cookies", wantBody: `{"action":"set","domain":"example.test","name":"sid","same_site":"lax","secure":true,"value":"v"}`},
		{name: "cookies delete", args: []string{"cookies", "delete", "sid", "--domain", ".example.test"}, wantMethod: http.MethodPost, wantPath: "/api/page/cookies", wantBody: `{"action":"delete","domain":".example.test","name":"sid"}`},
		{name: "network", args: []string{"network", "--filter", "api"}, wantMethod: http.MethodGet, wantPath: "/api/page/network_requests", wantQuery: url.Values{"filter": {"api"}}},
		{name: "clipboard write", args: []string{"clipboard", "write", "hello"}, wantMethod: http.MethodPost, wantPath: "/api/page/clipboard", wantBody: `{"action":"write","text":"hello"}`},
		{name: "artifact capture", args: []string{"artifact", "capture", "pdf"}, wantMethod: http.MethodPost, wantPath: "/api/artifacts/capture", wantBody: `{"kind":"pdf"}`},
		{name: "artifact search", args: []string{"artifact", "search", "art_1", "total", "--limit", "3"}, wantMethod: http.MethodPost, wantPath: "/api/artifacts/search", wantBody: `{"artifact_id":"art_1","limit":3,"query":"total"}`},
		{name: "recipe search", args: []string{"recipe", "search", "download invoices", "--origin", "https://app.example.test"}, wantMethod: http.MethodPost, wantPath: "/api/recipes/search", wantBody: `{"origin":"https://app.example.test","query":"download invoices"}`},
		{name: "notify", args: []string{"notify", "Need you", "Approve the payment", "--kind", "needs_input"}, wantMethod: http.MethodPost, wantPath: "/api/page/notify", wantBody: `{"kind":"needs_input","message":"Approve the payment","title":"Need you"}`},
		{name: "locale", args: []string{"locale", "en-GB", "--timezone", "Europe/London"}, wantMethod: http.MethodPost, wantPath: "/api/page/locale", wantBody: `{"locale":"en-GB","timezone":"Europe/London"}`},
		{name: "locale clear", args: []string{"locale", "--clear"}, wantMethod: http.MethodPost, wantPath: "/api/page/locale", wantBody: `{"clear":true}`},
		{name: "init-script add", args: []string{"init-script", "add", "window.x=1;"}, wantMethod: http.MethodPost, wantPath: "/api/page/init_script", wantBody: `{"action":"add","source":"window.x=1;"}`},
		{name: "init-script clear", args: []string{"init-script", "clear"}, wantMethod: http.MethodPost, wantPath: "/api/page/init_script", wantBody: `{"action":"clear"}`},
		{name: "batch inline", args: []string{"batch", `[{"action":"click","ref":"e1"}]`}, wantMethod: http.MethodPost, wantPath: "/api/page/batch", wantBody: `{"steps":[{"action":"click","ref":"e1"}]}`},
		{name: "batch from a file", args: []string{"batch", batchFile}, wantMethod: http.MethodPost, wantPath: "/api/page/batch", wantBody: `{"steps":[{"action":"press","key":"Enter"}]}`},
		{name: "tab close", args: []string{"tab", "close", "1234"}, wantMethod: http.MethodPost, wantPath: "/api/browser/close", wantBody: `{"id":"1234"}`},
		{name: "group", args: []string{"group", "1", "2", "--name", "research"}, wantMethod: http.MethodPost, wantPath: "/api/browser/group_tabs", wantBody: `{"name":"research","tab_ids":["1","2"]}`},
		{name: "incognito", args: []string{"incognito", "https://example.test/"}, wantMethod: http.MethodPost, wantPath: "/api/browser/open_incognito", wantBody: `{"url":"https://example.test/"}`, wantStdout: []string{"https://example.test/"}},
		{name: "close-context", args: []string{"close-context", "ctx-1"}, wantMethod: http.MethodPost, wantPath: "/api/browser/close_context", wantBody: `{"context_id":"ctx-1"}`},
		{name: "emulate", args: []string{"emulate", "iPhone 15", "--width", "390"}, wantMethod: http.MethodPost, wantPath: "/api/browser/emulate_device", wantBody: `{"device":"iPhone 15","width":390}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := fakeDaemon(t, moreResponses())
			t.Setenv("BRW_URL", srv.URL)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
			}
			if len(*calls) != 1 {
				t.Fatalf("daemon saw %d requests: %+v", len(*calls), *calls)
			}
			got := (*calls)[0]
			if got.method != tt.wantMethod || got.path != tt.wantPath {
				t.Fatalf("request = %s %s, want %s %s", got.method, got.path, tt.wantMethod, tt.wantPath)
			}
			if tt.wantQuery != nil && got.query.Encode() != tt.wantQuery.Encode() {
				t.Fatalf("query = %q, want %q", got.query.Encode(), tt.wantQuery.Encode())
			}
			if got.body != tt.wantBody {
				t.Fatalf("body = %s, want %s", got.body, tt.wantBody)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout %q lacks %q", stdout.String(), want)
				}
			}
			for _, unwanted := range tt.notStdout {
				if strings.Contains(stdout.String(), unwanted) {
					t.Fatalf("stdout %q carries %q", stdout.String(), unwanted)
				}
			}
		})
	}
}

func TestNewVerbsRejectBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"scroll", "sideways"},
		{"select", "@e1"},
		{"drag", "@e1"},
		{"clickxy", "1", "y"},
		{"cookies", "eat"},
		{"cookies", "set", "only-a-name"},
		{"locale"},
		{"locale", "--clear", "en-GB"},
		{"init-script", "add"},
		{"init-script", "run", "x"},
		{"batch", "[]"},
		{"batch", "[not json"},
		{"emulate"},
		{"emulate", "--clear", "iPhone 15"},
		{"group"},
		{"get"},
	} {
		srv, calls := fakeDaemon(t, moreResponses())
		t.Setenv("BRW_URL", srv.URL)
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != ExitUsage {
			t.Errorf("%q exited %d, want usage (stderr=%q)", args, code, stderr.String())
		}
		if len(*calls) != 0 {
			t.Errorf("%q reached the daemon", args)
		}
	}
}
