package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func harBytes(t *testing.T, requests []snapshot.CapturedRequest, redact bool) []byte {
	t.Helper()
	data, err := json.MarshalIndent(BuildHAR(requests, "https://example.com/", "Example", "0.0.0-test", redact), "", "  ")
	if err != nil {
		t.Fatalf("marshal har: %v", err)
	}
	return data
}

func TestParseHARFixtureRoundTripsAnExportedHAR(t *testing.T) {
	entries, err := ParseHARFixture(harBytes(t, sampleRequests(), true))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Method != "POST" || entry.URL != "https://api.example.com/login?next=/home" {
		t.Fatalf("request not round-tripped: %+v", entry)
	}
	if entry.Status != 200 || entry.Body != `{"ok":true}` {
		t.Fatalf("response not round-tripped: %+v", entry)
	}
	// Redaction is a property of the recording. A replay of a redacted HAR has
	// to carry the placeholder, not somehow recover the request body.
	if entry.RequestBody != redactedPlaceholder {
		t.Fatalf("request body = %q, want the recorded redaction placeholder", entry.RequestBody)
	}
	if strings.Contains(entry.RequestBody, "hunter2") {
		t.Fatal("a redacted HAR replayed the real request body")
	}
}

func TestParseHARFixtureRejectsWhatItCannotReplay(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "not json", data: []byte("not a har"), wantErr: "decode HAR"},
		{name: "not a har", data: []byte(`{"hello":"world"}`), wantErr: "no log.entries"},
		{name: "no entries", data: harBytes(t, nil, true), wantErr: "no request entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseHARFixture(tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// Replaying framing headers recorded against a different body would describe the
// response wrongly, and replaying Set-Cookie would write a recorded session into
// the profile running the fixture.
func TestParseHARFixtureDropsHeadersThatMustNotBeReplayed(t *testing.T) {
	raw := []byte(`{"log":{"version":"1.2","entries":[{"request":{"method":"GET","url":"https://x.test/a"},
		"response":{"status":200,"headers":[
			{"name":"Content-Length","value":"9999"},
			{"name":"Content-Encoding","value":"gzip"},
			{"name":"Set-Cookie","value":"session=recorded"},
			{"name":"X-Fixture","value":"kept"}],
		"content":{"mimeType":"application/json","text":"{}"}}}]}}`)
	entries, err := ParseHARFixture(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name := range entries[0].Headers {
		switch strings.ToLower(name) {
		case "content-length", "content-encoding", "set-cookie":
			t.Fatalf("header %q must not be replayed", name)
		}
	}
	if entries[0].Headers["X-Fixture"] != "kept" {
		t.Fatalf("ordinary headers should survive: %+v", entries[0].Headers)
	}
}

// A HAR is paged out of the artifact store like any other artifact, so the
// loader has to reassemble one that does not fit in a single read window.
func TestLoadHARFixtureReadsAStoredHARAcrossReadWindows(t *testing.T) {
	requests := make([]snapshot.CapturedRequest, 0, 400)
	for i := 0; i < 400; i++ {
		requests = append(requests, snapshot.CapturedRequest{
			Completed:       true,
			Method:          "GET",
			URL:             "https://api.example.com/item/" + strings.Repeat("x", 2000),
			Status:          200,
			OK:              true,
			ResponseSnippet: strings.Repeat("payload", 400),
		})
	}
	data := harBytes(t, requests, true)
	if len(data) <= MaxReadBytes {
		t.Fatalf("fixture HAR is %d bytes, which fits one read window and would not exercise paging", len(data))
	}

	store, err := NewStore(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	meta, err := store.Put(PutOptions{Kind: "har", MIMEType: "application/json"}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	svc, err := NewService(store, serviceFakeBrowser{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	entries, err := LoadHARFixture(context.Background(), svc, meta.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != len(requests) {
		t.Fatalf("loaded %d entries, want %d", len(entries), len(requests))
	}
}

// Pointing a replay at a screenshot would otherwise fail deep inside the JSON
// decoder with a message about the bytes rather than about the mistake.
func TestLoadHARFixtureRefusesAnArtifactOfAnotherKind(t *testing.T) {
	store, err := NewStore(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	meta, err := store.Put(PutOptions{Kind: "text", MIMEType: "text/plain"}, strings.NewReader("not a har"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	svc, err := NewService(store, serviceFakeBrowser{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	_, err = LoadHARFixture(context.Background(), svc, meta.ID)
	if err == nil || !strings.Contains(err.Error(), "not a har") {
		t.Fatalf("error = %v, want one naming the artifact kind", err)
	}
}
