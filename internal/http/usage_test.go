package httpapi

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestUsageMiddlewareNeverLogsRequestOrErrorText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.ndjson")
	recorder, err := usagelog.New(usagelog.Config{
		Path: path, MaxBytes: 1 << 20, Backups: 1,
		Identity: brwidentity.Identity{Workspace: "brw-test", Profile: "test", Mode: "bridge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{usage: recorder}
	h := s.usageMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, errors.New("password=SENSITIVE_SENTINEL_A token=TOKEN_SENTINEL_B"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/page/fill?secret=QUERY_SENTINEL_C", strings.NewReader(`{"text":"SENSITIVE_SENTINEL_A","url":"https://x.test/?token=TOKEN_SENTINEL_B"}`))
	req.Header.Set(usagelog.HeaderSessionID, "session-1")
	req.Header.Set(usagelog.HeaderRequestID, "session-1:4")
	req.Header.Set(usagelog.HeaderClient, "brw-httpclient")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SENSITIVE_SENTINEL_A", "TOKEN_SENTINEL_B", "QUERY_SENTINEL_C", "x.test", `"text"`, `"url"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("usage log retained %q: %s", forbidden, data)
		}
	}
	if !strings.Contains(string(data), `"operation":"brw_fill"`) || !strings.Contains(string(data), `"outcome":"error"`) {
		t.Fatalf("missing safe usage metadata: %s", data)
	}
	if body, _ := io.ReadAll(res.Result().Body); !strings.Contains(string(body), "SENSITIVE_SENTINEL_A") {
		t.Fatalf("middleware changed API error response: %s", body)
	}
}

func TestArtifactUsageLogNeverContainsHandleQueryOrBackingError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact-usage.ndjson")
	recorder, err := usagelog.New(usagelog.Config{
		Path: path, MaxBytes: 1 << 20, Backups: 1,
		Identity: brwidentity.Identity{Workspace: "brw-test", Profile: "test", Mode: "bridge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const artifactID = "art_0123456789abcdef0123456789abcdef"
	const query = "PRIVATE_ARTIFACT_QUERY_SENTINEL"
	api := &artifactAPIFake{operationErr: errors.New("backend failed for " + artifactID + " query " + query)}
	server := New("", &fakeController{})
	server.SetArtifactAPI(api)
	server.SetUsageRecorder(recorder)
	body := `{"artifact_id":"` + artifactID + `","query":"` + query + `","limit":1}`
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/artifacts/search", strings.NewReader(body)))
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{artifactID, query, "backend failed"} {
		if strings.Contains(string(data), forbidden) || strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("artifact HTTP telemetry or response retained %q: log=%s response=%s", forbidden, data, rec.Body.String())
		}
	}
	if !strings.Contains(string(data), `"operation":"brw_artifact_search"`) || !strings.Contains(string(data), `"outcome":"error"`) {
		t.Fatalf("missing safe artifact usage metadata: %s", data)
	}
	if !strings.Contains(string(data), `"error_class":"artifact_error"`) ||
		!strings.Contains(string(data), `"error_fingerprint":"`+usagelog.Fingerprint("artifact operation failed")+`"`) {
		t.Fatalf("artifact failure telemetry is not specific and stable: %s", data)
	}
}

// unloggedAPIRoutes are the /api/ routes deliberately outside usageOperations.
// Everything else is a tool call and belongs in the ledger; three reviews in a
// row found a newly added route missing from the allowlist, which is invisible
// rather than noisy - the middleware simply skips an unknown path.
var unloggedAPIRoutes = map[string]string{
	"/api/artifacts/{id}":        "wildcard handle route, classified by the middleware's /api/artifacts/ prefix fallback",
	"/api/artifacts/{id}/info":   "wildcard handle route, classified by the prefix fallback",
	"/api/artifacts/{id}/read":   "wildcard handle route, classified by the prefix fallback",
	"/api/artifacts/{id}/search": "wildcard handle route, classified by the prefix fallback",
	"/api/session/stream":        "long-lived SSE connection, not one operation with an outcome",
}

// routePathFromPattern reduces a net/http mux pattern - "[METHOD ][HOST]/[PATH]"
// - to the path the usage allowlist is keyed by.
//
// It reports failure rather than handing back the pattern unchanged because a
// pattern this parser cannot read is precisely the case the caller must not pass
// over: a shape it silently skipped would be a route missing from
// usageOperations that the guard declared fine.
func routePathFromPattern(pattern string) (string, bool) {
	rest := strings.TrimSpace(pattern)
	if method, after, found := strings.Cut(rest, " "); found {
		// The grammar allows nothing but a method before that space, and every
		// method this codebase registers is upper-case, which is also the only
		// spelling net/http will ever match a request against.
		if !isUpperCaseMethod(method) {
			return "", false
		}
		rest = strings.TrimSpace(after)
	}
	// The host is optional and ends at the first "/", which begins the path.
	if slash := strings.Index(rest, "/"); slash > 0 {
		rest = rest[slash:]
	}
	if !strings.HasPrefix(rest, "/") {
		return "", false
	}
	return rest, true
}

func isUpperCaseMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, r := range method {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// TestEveryAPIRouteIsInTheUsageAllowlist reads the route table out of server.go
// rather than a hand-kept list, so a route added tomorrow is covered too.
func TestEveryAPIRouteIsInTheUsageAllowlist(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		// Only the mux registrations: a HandleFunc called on anything else is
		// not a route this server serves.
		if receiver, ok := selector.X.(*ast.Ident); !ok || receiver.Name != "mux" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		path, ok := routePathFromPattern(pattern)
		if !ok {
			t.Errorf("route pattern %q could not be read, so this guard would skip the very route it exists to catch", pattern)
			return true
		}
		routes = append(routes, path)
		return true
	})
	if len(routes) < 50 {
		t.Fatalf("found %d routes in server.go, want the whole table; the parse is wrong", len(routes))
	}
	for _, route := range routes {
		if !strings.HasPrefix(route, "/api/") {
			continue
		}
		if usageOperations[route] != "" {
			continue
		}
		if reason := unloggedAPIRoutes[route]; reason != "" {
			continue
		}
		t.Errorf("route %s is in neither usageOperations nor unloggedAPIRoutes: every call to it is missing from the usage ledger", route)
	}
	for route := range usageOperations {
		if !slices.Contains(routes, route) {
			t.Errorf("usageOperations maps %s, which server.go no longer registers", route)
		}
	}
}

// The route guard is only as good as its reading of the mux patterns: a method
// it does not know must not turn into a route it quietly skips.
func TestRoutePathFromPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
		wantOK  bool
	}{
		{pattern: "GET /api/browser/tabs", want: "/api/browser/tabs", wantOK: true},
		{pattern: "POST /api/page/fill", want: "/api/page/fill", wantOK: true},
		{pattern: "DELETE /api/browser/tab", want: "/api/browser/tab", wantOK: true},
		// Not registered today. Registered tomorrow, it has to reach the
		// allowlist check rather than fall past both prefixes as "PUT /api/...".
		{pattern: "PUT /api/browser/window", want: "/api/browser/window", wantOK: true},
		{pattern: "PATCH /api/page/value", want: "/api/page/value", wantOK: true},
		{pattern: "OPTIONS /api/page/probe", want: "/api/page/probe", wantOK: true},
		{pattern: "/api/page/methodless", want: "/api/page/methodless", wantOK: true},
		{pattern: "GET brw.test/api/page/hosted", want: "/api/page/hosted", wantOK: true},
		{pattern: "  GET   /api/page/padded  ", want: "/api/page/padded", wantOK: true},
		// net/http matches the method case-sensitively, so a lower-case one never
		// serves anything; reading it as a path would hide that.
		{pattern: "get /api/page/lower"},
		{pattern: "GET nopathatall"},
		{pattern: ""},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got, ok := routePathFromPattern(tt.pattern)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("routePathFromPattern(%q) = (%q, %v), want (%q, %v)", tt.pattern, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
