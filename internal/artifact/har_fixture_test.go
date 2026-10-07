package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	kept := 0
	for _, header := range entries[0].Headers {
		switch strings.ToLower(header.Name) {
		case "content-length", "content-encoding", "set-cookie":
			t.Fatalf("header %q must not be replayed", header.Name)
		}
		if header.Name == "X-Fixture" && header.Value == "kept" {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("ordinary headers should survive: %+v", entries[0].Headers)
	}
}

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

type malformedChunkAPI struct {
	API
	chunk Chunk
	calls int
}

func (f *malformedChunkAPI) ArtifactInfo(context.Context, string) (Meta, error) {
	return Meta{Kind: "har", SizeBytes: 1}, nil
}
func (f *malformedChunkAPI) ReadArtifact(context.Context, string, int64, int) (Chunk, error) {
	f.calls++
	if f.calls > 1 {
		return Chunk{}, errors.New("read repeated without progress")
	}
	return f.chunk, nil
}
func TestLoadHARFixtureBoundsFinalChunkAndRequiresProgress(t *testing.T) {
	for _, test := range []struct {
		name  string
		chunk Chunk
		want  string
	}{
		{"non-progressing", Chunk{Encoding: "utf-8", Text: "x", SizeBytes: 1, More: true, NextOffset: 0}, "stopped returning bytes"},
		{"oversize final", Chunk{Encoding: "utf-8", Text: `{"log":{"version":"1.2","entries":[{"request":{"url":"https://x.test"},"response":{"content":{"text":"` + strings.Repeat("x", maxHARFixtureBytes) + `"}}}]}}`}, "replay limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &malformedChunkAPI{chunk: test.chunk}
			if _, err := LoadHARFixture(context.Background(), api, "fixture"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %s", err, test.want)
			}
			if api.calls != 1 {
				t.Fatalf("read %d times without validated progress", api.calls)
			}
		})
	}
}
