package usagelog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func TestRecorderWritesMetadataOnlyAndSecuresFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.ndjson")
	r, err := New(Config{
		Path: path, MaxBytes: 1 << 20, Backups: 2, Version: "0.7.0-test",
		Identity: brwidentity.Identity{Workspace: "brw-chromium", Profile: "chromium", Mode: "bridge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Record(Event{
		Layer: "http", Operation: "brw_fill", Outcome: "error", DurationMS: 12,
		ErrorClass: "tool", ErrorFingerprint: Fingerprint("password SENSITIVE_SENTINEL_A was rejected"),
		SessionID: "session-1", RequestID: "session-1:2", Client: "brw-httpclient",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"SENSITIVE_SENTINEL_A", "password SENSITIVE_SENTINEL_A", `"args"`, `"url"`, `"text"`, `"headers"`, `"body"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("usage log retained forbidden content %q: %s", forbidden, text)
		}
	}
	var event Event
	if err := json.Unmarshal(data[:len(data)-1], &event); err != nil {
		t.Fatal(err)
	}
	if event.Operation != "brw_fill" || event.Workspace != "brw-chromium" || event.ErrorFingerprint == "" {
		t.Fatalf("event = %+v", event)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestRecorderRotatesBoundedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.ndjson")
	r, err := New(Config{Path: path, MaxBytes: 220, Backups: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := r.Record(Event{Layer: "http", Operation: "brw_list_tabs", Outcome: "ok", SessionID: "session"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".1", path + ".2"} {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			var event Event
			if err := json.Unmarshal(s.Bytes(), &event); err != nil {
				t.Fatalf("invalid rotated JSON in %s: %v", name, err)
			}
		}
		_ = f.Close()
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third backup: %v", err)
	}
}

func TestSafeMetadataRejectsInjection(t *testing.T) {
	if got := SafeID("session\npassword=secret"); got != "" {
		t.Fatalf("SafeID = %q", got)
	}
	if got := SafeFingerprint("not-a-hash"); got != "" {
		t.Fatalf("SafeFingerprint = %q", got)
	}
	if got := ClassifyError(contextDeadlineError{}); got != "timeout" {
		t.Fatalf("ClassifyError = %q", got)
	}
	if a, b := Fingerprint("password SENSITIVE_SENTINEL_A was rejected"), Fingerprint("password SENSITIVE_SENTINEL_B was rejected"); a != b {
		t.Fatalf("fingerprint depends on caller-controlled secret: %q != %q", a, b)
	}
	groupingErr := errors.New("extension bridge: Grouping is not supported by tabs in this window.")
	if got := ClassifyError(groupingErr); got != "capability" {
		t.Fatalf("grouping ClassifyError = %q, want capability", got)
	}
	if got, want := Fingerprint(groupingErr.Error()), Fingerprint("tab grouping unavailable"); got != want {
		t.Fatalf("grouping fingerprint = %q, want stable capability fingerprint %q", got, want)
	}
}

type contextDeadlineError struct{}

func (contextDeadlineError) Error() string { return "operation timed out while waiting" }
