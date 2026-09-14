package browser

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeHARMatch(t *testing.T) {
	tests := []struct {
		name    string
		keys    []string
		want    []string
		wantErr string
	}{
		{name: "empty defaults to method and url", keys: nil, want: []string{"method", "url"}},
		{name: "blank entries are ignored", keys: []string{"", "  "}, want: []string{"method", "url"}},
		{name: "order is preserved", keys: []string{"url", "method"}, want: []string{"url", "method"}},
		{name: "case and spacing are tolerated", keys: []string{" URL "}, want: []string{"url"}},
		{name: "duplicates collapse", keys: []string{"url", "url"}, want: []string{"url"}},
		{name: "body is opt-in", keys: []string{"method", "url", "body"}, want: []string{"method", "url", "body"}},
		{name: "an unknown key is refused", keys: []string{"headers"}, wantErr: "unknown match key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeHARMatch(tt.keys)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeHARMiss(t *testing.T) {
	tests := []struct {
		value   string
		want    string
		wantErr string
	}{
		{value: "", want: HARMissPassthrough},
		{value: "passthrough", want: HARMissPassthrough},
		{value: " FAIL ", want: HARMissFail},
		{value: "explode", wantErr: "unknown on_miss"},
	}
	for _, tt := range tests {
		t.Run(tt.value+tt.want, func(t *testing.T) {
			got, err := NormalizeHARMiss(tt.value)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("on_miss = %q, want %q", got, tt.want)
			}
		})
	}
}

// The match keys decide which recordings answer which requests, so each key has
// to be load-bearing on its own: url alone must ignore the method, and body must
// tell two same-URL POSTs apart.
func TestHARFixtureMatchKeysSelectTheRecording(t *testing.T) {
	entries := []HAREntry{
		{Method: "GET", URL: "https://x.test/a", Status: 200, Body: "get-a"},
		{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":1}`, Status: 201, Body: "post-one"},
		{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":2}`, Status: 201, Body: "post-two"},
	}
	tests := []struct {
		name   string
		match  []string
		method string
		url    string
		body   string
		want   string
		found  bool
	}{
		{
			name: "method and url pick the GET", match: []string{HARMatchMethod, HARMatchURL},
			method: "GET", url: "https://x.test/a", want: "get-a", found: true,
		},
		{
			name: "method and url pick the first POST regardless of body", match: []string{HARMatchMethod, HARMatchURL},
			method: "POST", url: "https://x.test/a", body: `{"id":2}`, want: "post-one", found: true,
		},
		{
			name: "body disambiguates the two POSTs", match: []string{HARMatchMethod, HARMatchURL, HARMatchBody},
			method: "POST", url: "https://x.test/a", body: `{"id":2}`, want: "post-two", found: true,
		},
		{
			name: "url alone ignores the method", match: []string{HARMatchURL},
			method: "DELETE", url: "https://x.test/a", want: "get-a", found: true,
		},
		{
			name: "an unrecorded url misses", match: []string{HARMatchMethod, HARMatchURL},
			method: "GET", url: "https://x.test/missing",
		},
		{
			name: "an unrecorded body misses once body is a key", match: []string{HARMatchMethod, HARMatchURL, HARMatchBody},
			method: "POST", url: "https://x.test/a", body: `{"id":9}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newHARFixture("art-1", entries, tt.match, HARMissFail)
			got, found := fixture.find(tt.method, tt.url, tt.body)
			if found != tt.found {
				t.Fatalf("found = %v, want %v", found, tt.found)
			}
			if found && got.Body != tt.want {
				t.Fatalf("body = %q, want %q", got.Body, tt.want)
			}
		})
	}
}

// A HAR that recorded one URL twice holds two different answers, and replaying
// the first one forever would make a paged or polling page untestable.
func TestHARFixtureReplaysRepeatedRecordingsInOrder(t *testing.T) {
	fixture := newHARFixture("art-2", []HAREntry{
		{Method: "GET", URL: "https://x.test/poll", Body: "first"},
		{Method: "GET", URL: "https://x.test/poll", Body: "second"},
	}, []string{HARMatchMethod, HARMatchURL}, HARMissPassthrough)

	for _, want := range []string{"first", "second", "first"} {
		got, found := fixture.find("GET", "https://x.test/poll", "")
		if !found {
			t.Fatalf("a recorded URL should always answer, want %q", want)
		}
		if got.Body != want {
			t.Fatalf("body = %q, want %q", got.Body, want)
		}
	}
	if view := fixture.view(); view.Served != 3 || view.Missed != 0 {
		t.Fatalf("fixture view = %+v, want 3 served and no misses", view)
	}
}

func TestHARFixtureMissNamesTheRequestAndIsBounded(t *testing.T) {
	fixture := newHARFixture("art-3", []HAREntry{{Method: "GET", URL: "https://x.test/a"}},
		[]string{HARMatchMethod, HARMatchURL}, HARMissFail)

	miss := fixture.recordMiss("post", "https://x.test/nope")
	if miss.Method != "POST" {
		t.Fatalf("method = %q, want the normalized POST", miss.Method)
	}
	for _, want := range []string{"POST", "https://x.test/nope", "method url"} {
		if !strings.Contains(miss.Reason, want) {
			t.Fatalf("reason %q does not name %q", miss.Reason, want)
		}
	}
	for i := 0; i < maxRouteMisses+10; i++ {
		fixture.recordMiss("GET", "https://x.test/nope")
	}
	view := fixture.view()
	if len(view.Misses) != maxRouteMisses {
		t.Fatalf("kept %d misses, want the %d-entry bound", len(view.Misses), maxRouteMisses)
	}
	if view.Missed != maxRouteMisses+11 {
		t.Fatalf("missed count = %d, want every miss counted even once the list is trimmed", view.Missed)
	}
}

// A route pattern has to mean the same thing whichever transport enforces it:
// the direct-CDP backend matches it in Go, the extension bridge hands Chrome the
// regex this derives. Drift between the two is a mock that works on one browser
// and silently does not on the other.
func TestRoutePatternRegexAgreesWithTheGlobMatcher(t *testing.T) {
	patterns := []string{
		"*", "", "https://api.example.com/v1/*", "https://api.example.com/*",
		"*/users", "*.json", "https://api.example.com/search", "https://a.com/*/edit",
		"https://a.com/q?x=1*", "*/api/*",
	}
	urls := []string{
		"https://api.example.com/v1/users",
		"https://api.example.com/v1/users/42",
		"https://api.example.com/v2/users",
		"https://api.example.com/search?q=hi",
		"https://api.example.com/other",
		"https://cdn.example.com/data/config.json",
		"https://cdn.example.com/data/config.xml",
		"https://a.com/docs/42/edit",
		"https://a.com/q?x=1&y=2",
		"http://127.0.0.1:8080/api/items",
	}
	for _, pattern := range patterns {
		expr := RoutePatternRegex(pattern)
		compiled, err := regexp.Compile(expr)
		if err != nil {
			t.Fatalf("pattern %q produced an uncompilable regex %q: %v", pattern, expr, err)
		}
		for _, url := range urls {
			want := matchURLGlob(pattern, url)
			if got := compiled.MatchString(url); got != want {
				t.Errorf("pattern %q vs %q: regex %q says %v, matchURLGlob says %v", pattern, url, expr, got, want)
			}
		}
	}
}
