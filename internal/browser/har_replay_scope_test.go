package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

func replayRouteForTest(t *testing.T, entries []HAREntry, match []string, onMiss string) *Route {
	t.Helper()
	route, err := buildReplayRoute(RouteOptions{
		Action: "replay", HARArtifactID: "art-scope", HAR: entries, Match: match, OnMiss: onMiss,
	})
	if err != nil {
		t.Fatalf("build replay route: %v", err)
	}
	return route
}

func tableWith(t *testing.T, route *Route) *routeState {
	t.Helper()
	state := &routeState{}
	state.mu.Lock()
	state.initLocked()
	state.routes["tab-1"] = []*Route{route}
	state.mu.Unlock()
	return state
}

// A brw HAR is built from the in-page fetch/XHR wrappers, so it holds no
// document, script, stylesheet or image entry. A replay that claimed those would
// make the documented default pattern "*" with on_miss:"fail" refuse the
// navigation itself and leave a dead tab.
func TestReplayOnlyClaimsRequestKindsAHARCanHold(t *testing.T) {
	entries := []HAREntry{{Method: "GET", URL: "https://x.test/api", Status: 200, Body: "{}"}}
	tests := []struct {
		name     string
		resource network.ResourceType
		want     bool
	}{
		{name: "fetch is replayed", resource: network.ResourceTypeFetch, want: true},
		{name: "xhr is replayed", resource: network.ResourceTypeXHR, want: true},
		{name: "an unclassified request is replayed", resource: "", want: true},
		{name: "the document is not", resource: network.ResourceTypeDocument},
		{name: "a script is not", resource: network.ResourceTypeScript},
		{name: "a stylesheet is not", resource: network.ResourceTypeStylesheet},
		{name: "an image is not", resource: network.ResourceTypeImage},
		{name: "a font is not", resource: network.ResourceTypeFont},
		{name: "a CORS preflight is not", resource: network.ResourceTypePreflight},
		{name: "an EventSource is not", resource: network.ResourceTypeEventSource},
		{name: "a WebSocket is not", resource: network.ResourceTypeWebSocket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := replayRouteForTest(t, entries, nil, HARMissFail)
			state := tableWith(t, route)
			hit := state.match("tab-1", "https://x.test/api", tt.resource)
			if (hit != nil) != tt.want {
				t.Fatalf("match = %v, want claimed=%v", hit != nil, tt.want)
			}
			view := route.har.view()
			if tt.want {
				if view.NotReplayable != 0 {
					t.Fatalf("a replayed request should not be counted as not replayable: %+v", view)
				}
				return
			}
			if view.NotReplayable != 1 {
				t.Fatalf("a passed-through request must be counted, got %+v", view)
			}
			if route.Matched != 0 {
				t.Fatalf("matched = %d; a request the fixture declined must not spend the rule's budget", route.Matched)
			}
		})
	}
}

// A times budget spent on the document would retire the rule before the first
// API call the fixture exists to answer.
func TestReplayTimesBudgetIsNotSpentOnADocument(t *testing.T) {
	route := replayRouteForTest(t, []HAREntry{{Method: "GET", URL: "https://x.test/api", Status: 200}}, nil, HARMissFail)
	route.Times = 1
	state := tableWith(t, route)

	if hit := state.match("tab-1", "https://x.test/api", network.ResourceTypeDocument); hit != nil {
		t.Fatal("the document must not be claimed by a replay route")
	}
	if hit := state.match("tab-1", "https://x.test/api", network.ResourceTypeFetch); hit == nil {
		t.Fatal("the fetch the rule exists for should still match")
	}
}

// An export redacts request bodies unless it was taken with redaction:"none", so
// a body-keyed replay of an ordinary capture matches nothing and, under the
// default on_miss:"passthrough", runs the whole test against the real backend.
func TestReplayRefusesABodyKeyedMatchOnARedactedCapture(t *testing.T) {
	redacted := []HAREntry{
		{Method: "POST", URL: "https://x.test/a", RequestBody: HARRedactedPlaceholder},
		{Method: "POST", URL: "https://x.test/b", RequestBody: HARRedactedPlaceholder},
	}
	tests := []struct {
		name    string
		entries []HAREntry
		match   []string
		wantErr []string
	}{
		{
			name: "body keyed on a redacted capture is refused", entries: redacted,
			match:   []string{HARMatchMethod, HARMatchURL, HARMatchBody},
			wantErr: []string{"body", HARRedactedPlaceholder, "redaction:\"none\"", "art-scope"},
		},
		{
			name: "the same capture without body as a key is fine", entries: redacted,
			match: []string{HARMatchMethod, HARMatchURL},
		},
		{
			name: "body keyed on an unredacted capture is fine",
			entries: []HAREntry{
				{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":1}`},
				{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":2}`},
			},
			match: []string{HARMatchMethod, HARMatchURL, HARMatchBody},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildReplayRoute(RouteOptions{
				Action: "replay", HARArtifactID: "art-scope", HAR: tt.entries, Match: tt.match,
			})
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a body-keyed replay of a redacted capture was accepted; every request would have missed")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// brw's capture clips a response at 2 KiB. Serving that as though it were the
// whole body reaches the page as a syntax error that points at the page, so the
// fixture has to say how many of its recordings are snippets.
func TestReplayReportsTruncatedRecordings(t *testing.T) {
	entries := []HAREntry{
		{Method: "GET", URL: "https://x.test/small", Status: 200, Body: "{}"},
		{Method: "GET", URL: "https://x.test/big", Status: 200, Body: "{\"a\":\"bbb", Truncated: true},
	}
	route := replayRouteForTest(t, entries, nil, HARMissFail)

	view := route.har.view()
	if view.Truncated != 1 || view.ServedTruncated != 0 {
		t.Fatalf("fixture view = %+v, want one truncated entry and none served yet", view)
	}
	note := harReplayNote(route.har)
	for _, want := range []string{"1 of 2", "truncated"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the replay note %q does not report the truncated recordings (%q)", note, want)
		}
	}

	if _, found := route.har.find("GET", "https://x.test/small", ""); !found {
		t.Fatal("the whole recording should answer")
	}
	if view := route.har.view(); view.ServedTruncated != 0 {
		t.Fatalf("serving a whole body counted as truncated: %+v", view)
	}
	if _, found := route.har.find("GET", "https://x.test/big", ""); !found {
		t.Fatal("the truncated recording still answers; it is reported, not withheld")
	}
	if view := route.har.view(); view.ServedTruncated != 1 {
		t.Fatalf("fixture view = %+v, want the truncated body counted as served", view)
	}
}

// The note is what an agent reads before driving the page, so it has to name the
// one thing the pattern does not cover.
func TestReplayNoteSaysWhatTheRecordingCannotAnswer(t *testing.T) {
	route := replayRouteForTest(t, []HAREntry{{Method: "GET", URL: "https://x.test/api"}}, nil, HARMissFail)
	note := harReplayNote(route.har)
	for _, want := range []string{"fetch and XHR", "document", "refused"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the replay note %q does not say %q", note, want)
		}
	}
	passthrough := replayRouteForTest(t, []HAREntry{{Method: "GET", URL: "https://x.test/api"}}, nil, HARMissPassthrough)
	if !strings.Contains(harReplayNote(passthrough.har), "goes to the real network") {
		t.Fatalf("the passthrough note does not say where a miss goes: %q", harReplayNote(passthrough.har))
	}
}

// A closed tab's routes pin the whole decoded HAR - entries, bodies and all -
// for the life of the daemon if they are not dropped with the rest of the
// per-tab state.
func TestForgetTabCachesDropsTheRouteTableAndContainment(t *testing.T) {
	m := &Manager{}
	route := replayRouteForTest(t, []HAREntry{{Method: "GET", URL: "https://x.test/api", Body: "recorded"}}, nil, HARMissFail)

	m.routes.mu.Lock()
	m.routes.initLocked()
	m.routes.routes["tab-gone"] = []*Route{route}
	m.routes.mu.Unlock()

	m.containment.mu.Lock()
	m.containment.initLocked()
	m.containment.armed["tab-gone"] = true
	m.containment.blocked["tab-gone"] = []BlockedRequest{{URL: "https://x.test/blocked"}}
	m.containment.mu.Unlock()

	m.forgetTabCaches("tab-gone")

	if got := m.routes.count("tab-gone"); got != 0 {
		t.Fatalf("the closed tab still holds %d route(s), pinning its HAR", got)
	}
	m.routes.mu.Lock()
	_, stillThere := m.routes.routes["tab-gone"]
	m.routes.mu.Unlock()
	if stillThere {
		t.Fatal("the closed tab's route-table entry survived; the map grows by one on every closed tab")
	}
	m.containment.mu.Lock()
	armed := m.containment.armed["tab-gone"]
	blocked := m.containment.blocked["tab-gone"]
	m.containment.mu.Unlock()
	if armed || blocked != nil {
		t.Fatalf("containment state survived the closed tab: armed=%v blocked=%v", armed, blocked)
	}
}

// brw_observe reports active routes as a bare count, so a fixture that is
// missing every request looks exactly like one that is answering them.
func TestRouteMissSummaryNamesWhatTheFixtureCouldNotAnswer(t *testing.T) {
	route := replayRouteForTest(t, []HAREntry{{Method: "GET", URL: "https://x.test/api"}}, nil, HARMissFail)
	state := tableWith(t, route)

	if missed, reasons := state.missSummary("tab-1", maxObservedRouteMissReasons); missed != 0 || reasons != nil {
		t.Fatalf("summary = %d %v, want nothing before any miss", missed, reasons)
	}
	for _, url := range []string{"https://x.test/one", "https://x.test/two", "https://x.test/three", "https://x.test/four"} {
		route.har.recordMiss("GET", url)
	}
	missed, reasons := state.missSummary("tab-1", maxObservedRouteMissReasons)
	if missed != 4 {
		t.Fatalf("missed = %d, want every miss counted", missed)
	}
	if len(reasons) != maxObservedRouteMissReasons {
		t.Fatalf("reported %d reasons, want the %d-entry bound", len(reasons), maxObservedRouteMissReasons)
	}
	if !strings.Contains(reasons[0], "/four") {
		t.Fatalf("the most recent miss is not reported first: %q", reasons[0])
	}
}

// answerRoute runs on the Fetch.requestPaused goroutine, which has no recover:
// a nil dereference there takes the whole daemon down rather than failing one
// request. Nothing in brw_route can build a replay route without a fixture
// today, so this asserts the guard rather than a live path.
func TestAnswerRouteOnAReplayWithoutAFixtureDoesNotPanic(t *testing.T) {
	m := &Manager{}
	paused := &fetch.EventRequestPaused{
		RequestID:    fetch.RequestID("req-1"),
		Request:      &network.Request{URL: "https://x.test/api", Method: "GET"},
		ResourceType: network.ResourceTypeFetch,
	}
	// A context with no CDP executor: continueWithEnvironmentHeaders reports that
	// as an error, which is the failure mode this has to degrade to.
	if err := m.answerRoute(context.Background(), "tab-1", &Route{Behaviour: RouteReplay}, paused); err == nil {
		t.Fatal("continuing a request with no CDP executor should report an error")
	}
}
