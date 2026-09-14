package artifact

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// The in-page capture keeps the first 2 KiB of a response. Replaying that as
// though it were the whole body hands the page a JSON document that stops
// mid-string, and the resulting SyntaxError names the page rather than the
// fixture, so the clip has to be identified when the HAR is decoded.
func TestParseHARFixtureFlagsAClippedResponseBody(t *testing.T) {
	clipped := strings.Repeat("x", 2048) + snapshot.BodyTruncationMarker
	tests := []struct {
		name    string
		request snapshot.CapturedRequest
		want    bool
	}{
		{
			name: "a body the capture clipped is flagged",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "GET", URL: "https://x.test/big", Status: 200, OK: true,
				ResponseSnippet: clipped,
			},
			want: true,
		},
		{
			name: "a body that fit is not",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "GET", URL: "https://x.test/small", Status: 200, OK: true,
				ResponseSnippet: `{"ok":true}`,
			},
		},
		{
			name: "an empty body is not",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "GET", URL: "https://x.test/empty", Status: 204, OK: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ParseHARFixture(harBytes(t, []snapshot.CapturedRequest{tt.request}, true))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if entries[0].Truncated != tt.want {
				t.Fatalf("truncated = %v, want %v for %q", entries[0].Truncated, tt.want, entries[0].Body)
			}
		})
	}
}

// An externally produced HAR states the real body size instead of appending a
// marker, so a text shorter than the size it declares is the same fact.
func TestParseHARFixtureFlagsABodyShorterThanItsDeclaredSize(t *testing.T) {
	raw := []byte(`{"log":{"version":"1.2","entries":[
		{"request":{"method":"GET","url":"https://x.test/partial"},
		 "response":{"status":200,"headers":[],"content":{"size":4096,"mimeType":"application/json","text":"{\"a\":1"}}},
		{"request":{"method":"GET","url":"https://x.test/whole"},
		 "response":{"status":200,"headers":[],"content":{"size":6,"mimeType":"application/json","text":"{\"a\":1}"}}}]}}`)
	entries, err := ParseHARFixture(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !entries[0].Truncated {
		t.Fatal("a body shorter than its declared content.size is a partial recording")
	}
	if entries[1].Truncated {
		t.Fatal("a body at least as long as its declared size is whole")
	}
}

// HAR 1.2 stores headers as a list because Link, Vary and Www-Authenticate may
// legally repeat. Collapsing them into a map replays only the last one.
func TestParseHARFixtureKeepsRepeatedResponseHeaders(t *testing.T) {
	raw := []byte(`{"log":{"version":"1.2","entries":[{"request":{"method":"GET","url":"https://x.test/a"},
		"response":{"status":200,"headers":[
			{"name":"Link","value":"</one>; rel=preload"},
			{"name":"Set-Cookie","value":"session=recorded"},
			{"name":"Link","value":"</two>; rel=preload"},
			{"name":"Vary","value":"Accept"},
			{"name":"Vary","value":"Origin"}],
		"content":{"mimeType":"application/json","text":"{}"}}}]}}`)
	entries, err := ParseHARFixture(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var links, varys []string
	for _, header := range entries[0].Headers {
		switch header.Name {
		case "Link":
			links = append(links, header.Value)
		case "Vary":
			varys = append(varys, header.Value)
		case "Set-Cookie":
			t.Fatal("Set-Cookie must not be replayed into the profile running the fixture")
		}
	}
	if len(links) != 2 || links[0] != "</one>; rel=preload" || links[1] != "</two>; rel=preload" {
		t.Fatalf("Link headers = %v, want both in recorded order", links)
	}
	if len(varys) != 2 {
		t.Fatalf("Vary headers = %v, want both", varys)
	}
}

// The capture clips a REQUEST body at the same 2 KiB cap as a response, and a
// request body is a match key. Served as though it were whole, the recording is
// a prefix no live request can equal, so a body-keyed replay of such a capture
// misses every request and - under the default on_miss:"passthrough" - runs the
// whole test against the real backend. The clip has to be identified when the
// HAR is decoded so the install can be refused instead.
func TestParseHARFixtureFlagsAClippedRequestBody(t *testing.T) {
	clipped := strings.Repeat("p", snapshot.BodyCapBytes) + snapshot.BodyTruncationMarker
	tests := []struct {
		name    string
		request snapshot.CapturedRequest
		want    bool
	}{
		{
			name: "a request body the capture clipped is flagged",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "POST", URL: "https://x.test/big", Status: 200, OK: true,
				RequestBody: clipped,
			},
			want: true,
		},
		{
			name: "a request body that fit is not",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "POST", URL: "https://x.test/small", Status: 200, OK: true,
				RequestBody: `{"id":1}`,
			},
		},
		{
			name: "a request with no body is not",
			request: snapshot.CapturedRequest{
				Completed: true, Method: "GET", URL: "https://x.test/none", Status: 200, OK: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ParseHARFixture(harBytes(t, []snapshot.CapturedRequest{tt.request}, false))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if entries[0].RequestBodyTruncated != tt.want {
				t.Fatalf("request_body_truncated = %v, want %v for %q", entries[0].RequestBodyTruncated, tt.want, entries[0].RequestBody)
			}
		})
	}
}

// An externally produced HAR states the request's real bodySize rather than
// appending a marker, so a postData.text shorter than that is the same fact.
func TestParseHARFixtureFlagsARequestBodyShorterThanItsDeclaredSize(t *testing.T) {
	raw := []byte(`{"log":{"version":"1.2","entries":[
		{"request":{"method":"POST","url":"https://x.test/partial","bodySize":4096,
		  "postData":{"mimeType":"application/json","text":"{\"a\":1"}},
		 "response":{"status":200,"headers":[],"content":{"mimeType":"application/json","text":"{}"}}},
		{"request":{"method":"POST","url":"https://x.test/whole","bodySize":6,
		  "postData":{"mimeType":"application/json","text":"{\"a\":1}"}},
		 "response":{"status":200,"headers":[],"content":{"mimeType":"application/json","text":"{}"}}}]}}`)
	entries, err := ParseHARFixture(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !entries[0].RequestBodyTruncated {
		t.Fatal("a request body shorter than its declared bodySize is a partial recording")
	}
	if entries[1].RequestBodyTruncated {
		t.Fatal("a request body at least as long as its declared bodySize is whole")
	}
}
