package browser

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

// The in-page capture clips a request body at the same cap as a response, and
// unlike a response body a request body is a MATCH KEY: the recording holds a
// 2 KiB prefix and the live page sends the whole thing, so every such request
// misses and, under the default on_miss:"passthrough", reaches the real backend
// while the caller believes the page is mocked. Refusing at install is the only
// point at which the caller can still do something about it.
func TestReplayRefusesABodyKeyedMatchOnAClippedCapture(t *testing.T) {
	clipped := strings.Repeat("p", snapshot.BodyCapBytes) + snapshot.BodyTruncationMarker
	tests := []struct {
		name    string
		entries []HAREntry
		match   []string
		wantErr []string
	}{
		{
			name: "body keyed on a clipped capture is refused",
			entries: []HAREntry{
				{Method: "POST", URL: "https://x.test/a", RequestBody: clipped, RequestBodyTruncated: true},
				{Method: "POST", URL: "https://x.test/b", RequestBody: `{"id":2}`},
			},
			match:   []string{HARMatchMethod, HARMatchURL, HARMatchBody},
			wantErr: []string{"body", "1 of 2", "2048", snapshot.BodyTruncationMarker, "art-scope"},
		},
		{
			name: "the same capture without body as a key is fine",
			entries: []HAREntry{
				{Method: "POST", URL: "https://x.test/a", RequestBody: clipped, RequestBodyTruncated: true},
			},
			match: []string{HARMatchMethod, HARMatchURL},
		},
		{
			name: "body keyed on whole request bodies is fine",
			entries: []HAREntry{
				{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":1}`},
				{Method: "POST", URL: "https://x.test/a", RequestBody: `{"id":2}`},
			},
			match: []string{HARMatchMethod, HARMatchURL, HARMatchBody},
		},
		{
			// A redacted capture is also clipped when it is large, and the caller's
			// next move differs: recapture with redaction:"none" rather than give up
			// on body matching, so the redaction is what gets named.
			name: "a redacted capture is still reported as redacted",
			entries: []HAREntry{
				{Method: "POST", URL: "https://x.test/a", RequestBody: HARRedactedPlaceholder},
				{Method: "POST", URL: "https://x.test/b", RequestBody: clipped, RequestBodyTruncated: true},
			},
			match:   []string{HARMatchMethod, HARMatchURL, HARMatchBody},
			wantErr: []string{HARRedactedPlaceholder, "redaction:\"none\""},
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
				t.Fatal("a body-keyed replay of a clipped capture was accepted; every oversized request would have missed")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func postDataEntries(chunks ...string) []*network.PostDataEntry {
	out := make([]*network.PostDataEntry, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, &network.PostDataEntry{Bytes: base64.StdEncoding.EncodeToString([]byte(chunk))})
	}
	return out
}

// CDP hands a request body over as base64 chunks and omits them for a body it
// considers too long, while still setting hasPostData. Reassembling has to say
// which of those two happened: "" as a body is a real value that a recorded
// entry can match.
func TestRequestPostDataSaysWhenChromeWithheldTheBody(t *testing.T) {
	tests := []struct {
		name         string
		request      *network.Request
		want         string
		wantReadable bool
	}{
		{name: "no request at all", request: nil, wantReadable: true},
		{
			name:         "a request that carries no body",
			request:      &network.Request{Method: "GET", URL: "https://x.test/a"},
			wantReadable: true,
		},
		{
			name:         "one chunk",
			request:      &network.Request{Method: "POST", PostDataEntries: postDataEntries(`{"id":1}`), HasPostData: true},
			want:         `{"id":1}`,
			wantReadable: true,
		},
		{
			name:         "chunks are joined in order",
			request:      &network.Request{Method: "POST", PostDataEntries: postDataEntries(`{"id`, `":1}`), HasPostData: true},
			want:         `{"id":1}`,
			wantReadable: true,
		},
		{
			name: "a nil chunk is skipped rather than fatal",
			request: &network.Request{
				Method:          "POST",
				PostDataEntries: append([]*network.PostDataEntry{nil}, postDataEntries(`{"id":1}`)...),
				HasPostData:     true,
			},
			want:         `{"id":1}`,
			wantReadable: true,
		},
		{
			name:    "a body Chrome judged too long is withheld, not empty",
			request: &network.Request{Method: "POST", HasPostData: true},
		},
		{
			name: "an element with no bytes is withheld too",
			request: &network.Request{
				Method:          "POST",
				PostDataEntries: []*network.PostDataEntry{{}},
				HasPostData:     true,
			},
		},
		{
			name: "undecodable bytes are withheld, not silently dropped",
			request: &network.Request{
				Method:          "POST",
				PostDataEntries: []*network.PostDataEntry{{Bytes: "not base64!!"}},
				HasPostData:     true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, readable := requestPostData(tt.request)
			if body != tt.want || readable != tt.wantReadable {
				t.Fatalf("requestPostData = %q, %v; want %q, %v", body, readable, tt.want, tt.wantReadable)
			}
		})
	}
}

// A body Chrome withheld read as "" and was looked up as one, so a fixture
// holding a recording of a request that genuinely had no body answered an
// unrelated oversized POST with it. A wrong recorded answer is worse than a
// miss, and the miss has to say that the body — not the recording — is what was
// missing, because the caller's fix is to drop "body" from match.
func TestReplayDoesNotAnswerAWithheldRequestBodyFromAnEmptyRecording(t *testing.T) {
	entries := []HAREntry{{Method: "POST", URL: "https://x.test/api", Status: 200, Body: "recorded-for-the-empty-body"}}
	bodyMatch := []string{HARMatchMethod, HARMatchURL, HARMatchBody}
	tests := []struct {
		name        string
		request     *network.Request
		wantServed  int
		wantMissed  int
		wantInMiss  []string
		description string
	}{
		{
			name:       "a withheld body misses by name",
			request:    &network.Request{Method: "POST", URL: "https://x.test/api", HasPostData: true},
			wantMissed: 1,
			wantInMiss: []string{"POST", "https://x.test/api", HARMatchBody, "Chrome delivered no request body"},
		},
		{
			name:       "a request that really has no body still matches the recording",
			request:    &network.Request{Method: "POST", URL: "https://x.test/api"},
			wantServed: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			route := replayRouteForTest(t, entries, bodyMatch, HARMissFail)
			paused := &fetch.EventRequestPaused{
				RequestID:    fetch.RequestID("req-1"),
				Request:      tt.request,
				ResourceType: network.ResourceTypeFetch,
			}
			// No CDP executor, so answering fails either way; the fixture counters
			// are what say which answer was chosen.
			_ = m.answerRoute(context.Background(), "tab-1", route, paused)

			view := route.har.view()
			if view.Served != tt.wantServed {
				t.Fatalf("served = %d, want %d: %+v", view.Served, tt.wantServed, view)
			}
			if view.Missed != tt.wantMissed {
				t.Fatalf("missed = %d, want %d: %+v", view.Missed, tt.wantMissed, view)
			}
			if len(tt.wantInMiss) == 0 {
				return
			}
			if len(view.Misses) != 1 {
				t.Fatalf("recorded %d misses, want exactly the withheld body", len(view.Misses))
			}
			for _, want := range tt.wantInMiss {
				if !strings.Contains(view.Misses[0].Reason, want) {
					t.Fatalf("the miss reason %q does not name %q", view.Misses[0].Reason, want)
				}
			}
		})
	}
}

// Only a body-keyed replay needs the body, so a withheld one must not turn a
// fixture keyed on [method,url] into a miss: the recording answers it fine.
func TestReplayIgnoresAWithheldBodyWhenBodyIsNotAMatchKey(t *testing.T) {
	m := &Manager{}
	route := replayRouteForTest(t, []HAREntry{{Method: "POST", URL: "https://x.test/api", Status: 200, Body: "{}"}}, nil, HARMissFail)
	paused := &fetch.EventRequestPaused{
		RequestID:    fetch.RequestID("req-1"),
		Request:      &network.Request{Method: "POST", URL: "https://x.test/api", HasPostData: true},
		ResourceType: network.ResourceTypeFetch,
	}
	_ = m.answerRoute(context.Background(), "tab-1", route, paused)

	if view := route.har.view(); view.Served != 1 || view.Missed != 0 {
		t.Fatalf("fixture view = %+v; a fixture keyed on [method url] does not need the body", view)
	}
}
