package artifact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func sampleRequests() []snapshot.CapturedRequest {
	return []snapshot.CapturedRequest{
		{
			Completed: true,
			Method:    "POST",
			URL:       "https://api.example.com/login?next=/home",
			RequestHeaders: map[string]string{
				"Content-Type":  "application/json",
				"Cookie":        "session=super-secret",
				"Authorization": "Bearer super-secret-token",
				"X-Api-Key":     "key-12345",
			},
			RequestBody:     `{"password":"hunter2"}`,
			Status:          200,
			OK:              true,
			ResponseSnippet: `{"ok":true}`,
			StartedAt:       120.5,
			DurationMS:      42.25,
		},
	}
}

func TestBuildHARRedactsCredentialsByDefault(t *testing.T) {
	log := BuildHAR(sampleRequests(), "https://example.com/", "Example", "0.13.0", true)
	encoded, err := json.Marshal(log)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(encoded)

	for _, secret := range []string{"super-secret", "super-secret-token", "key-12345", "hunter2"} {
		if strings.Contains(text, secret) {
			t.Errorf("a redacted HAR still contains %q; a HAR is meant to be shared", secret)
		}
	}
	if !strings.Contains(text, redactedPlaceholder) {
		t.Error("redacted values should be visibly marked, not silently dropped")
	}
	// Non-credential headers must survive, or the export is useless.
	if !strings.Contains(text, "application/json") {
		t.Error("ordinary headers should be preserved")
	}
	if !strings.Contains(log.Log.Comment, "redacted") {
		t.Error("the log comment should state that redaction was applied")
	}
}

func TestBuildHARCanExportUnredacted(t *testing.T) {
	log := BuildHAR(sampleRequests(), "https://example.com/", "Example", "0.13.0", false)
	encoded, _ := json.Marshal(log)
	text := string(encoded)
	if !strings.Contains(text, "super-secret-token") {
		t.Error("redaction=none should export the real header values")
	}
	if !strings.Contains(log.Log.Comment, "REDACTION DISABLED") {
		t.Error("an unredacted export must say so loudly in the file itself")
	}
}

func TestBuildHARShape(t *testing.T) {
	log := BuildHAR(sampleRequests(), "https://example.com/", "Example", "0.13.0", true)
	if log.Log.Version != "1.2" {
		t.Errorf("version = %q, want 1.2", log.Log.Version)
	}
	if log.Log.Creator.Name != "brw" || log.Log.Creator.Version != "0.13.0" {
		t.Errorf("creator = %+v, want brw/0.13.0", log.Log.Creator)
	}
	if len(log.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(log.Log.Entries))
	}
	entry := log.Log.Entries[0]
	if entry.Request.Method != "POST" {
		t.Errorf("method = %q", entry.Request.Method)
	}
	if entry.Response.Status != 200 {
		t.Errorf("status = %d", entry.Response.Status)
	}
	if entry.Time != 42.25 {
		t.Errorf("time = %v, want 42.25", entry.Time)
	}
	// Query strings are parsed out, as HAR consumers expect.
	if len(entry.Request.QueryString) != 1 || entry.Request.QueryString[0].Name != "next" {
		t.Errorf("queryString = %+v, want one entry named next", entry.Request.QueryString)
	}
	// brw measures one duration, so the phases it did not measure are -1
	// (unavailable) rather than 0 (measured as instant).
	if entry.Timings.Send != -1 || entry.Timings.Receive != -1 {
		t.Errorf("unmeasured timings should be -1, got %+v", entry.Timings)
	}
	if entry.Timings.Wait != 42.25 {
		t.Errorf("wait = %v, want the measured duration", entry.Timings.Wait)
	}
	if entry.StartedDateTime == "" {
		t.Error("startedDateTime must be an absolute timestamp")
	}
}

func TestBuildHARHeaderOrderIsStable(t *testing.T) {
	first, _ := json.Marshal(BuildHAR(sampleRequests(), "u", "t", "v", true).Log.Entries[0].Request.Headers)
	for i := 0; i < 5; i++ {
		again, _ := json.Marshal(BuildHAR(sampleRequests(), "u", "t", "v", true).Log.Entries[0].Request.Headers)
		if string(first) != string(again) {
			t.Fatal("header order must be stable so two exports of one capture compare equal")
		}
	}
}

func TestBuildHARMarksIncompleteRequests(t *testing.T) {
	requests := sampleRequests()
	requests[0].Completed = false
	requests[0].Error = "net::ERR_TIMED_OUT"
	log := BuildHAR(requests, "u", "t", "v", true)
	comment := log.Log.Entries[0].Comment
	if !strings.Contains(comment, "had not completed") || !strings.Contains(comment, "ERR_TIMED_OUT") {
		t.Errorf("comment = %q, want it to record both the incompleteness and the error", comment)
	}
}
