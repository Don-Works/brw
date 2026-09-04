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
		if err := s.Err(); err != nil {
			t.Fatalf("scan %s: %v", name, err)
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
	leaseErr := errors.New("tab is leased by another browser session until 2026-07-16T12:00:00Z")
	if got := ClassifyError(leaseErr); got != "tab_contended" {
		t.Fatalf("lease ClassifyError = %q, want tab_contended", got)
	}
	if Retryable(ClassifyError(leaseErr)) {
		t.Fatal("tab contention must not be marked retryable")
	}
	frozenErr := errors.New("tab 41 is frozen by Chrome (collapsed tab group or Energy Saver) and could not be revived; expand its tab group or focus it once, then retry")
	if got := ClassifyError(frozenErr); got != "tab_frozen" {
		t.Fatalf("frozen ClassifyError = %q, want tab_frozen", got)
	}
	if got, want := Fingerprint(frozenErr.Error()), Fingerprint("tab 7 is frozen by Chrome"); got != want {
		t.Fatalf("frozen fingerprint = %q, want stable %q", got, want)
	}
	discardedErr := errors.New("tab 41 was discarded by Chrome (Memory Saver) and could not be revived by reload; reopen the page with brw_open")
	if got := ClassifyError(discardedErr); got != "tab_discarded" {
		t.Fatalf("discarded ClassifyError = %q, want tab_discarded", got)
	}
	if Retryable(ClassifyError(frozenErr)) || Retryable(ClassifyError(discardedErr)) {
		t.Fatal("unrevivable frozen/discarded tabs must not be marked retryable")
	}
}

type contextDeadlineError struct{}

func (contextDeadlineError) Error() string { return "operation timed out while waiting" }

// TestFingerprintCoversCommonAgentFailures pins the failure shapes for the
// errors brw actually raises most often. Before these arms existed, 78% of
// tool errors in the usage ledger hashed to the "other" catch-all — including
// every brw_click_text and brw_emulate_device failure — so the ledger could
// not tell an operator which tools were failing agents or why.
func TestFingerprintCoversCommonAgentFailures(t *testing.T) {
	// Fingerprint of a message matching no arm at all: the "other" bucket.
	other := Fingerprint("wholly unrecognised failure text")

	// Real strings, copied from the sites that construct them.
	cases := map[string]string{
		"stale ref (not recoverable)": `element ref "e999" not recoverable: no_key`,
		"text not found":              `click text: no visible element found for text "Sign in"`,
		"bad device preset":           `unknown device preset "iPhone 15"; use iphone_se, iphone_14, responsive, or explicit width/height`,
		"runtime exception":           `runtime exception: Error: boom`,
		"no current window":           `extension bridge: No current window`,
		// Verbatim from Chrome when brw resolves a foreign extension's page as the
		// foreground tab — a password-manager popout holding focus. This recurred
		// all day in the real ledger while sitting in the "other" bucket.
		"foreign extension page": `extension bridge: Cannot access a chrome-extension:// URL of different extension`,
		"devtools owns the tab":  `cannot control tab 42: another debugger (likely DevTools) is already attached; close DevTools on that tab and retry`,
	}
	seen := map[string]string{}
	for name, msg := range cases {
		fp := Fingerprint(msg)
		if fp == other {
			t.Errorf("%s still collapses into the \"other\" bucket: %q", name, msg)
		}
		if prev, dup := seen[fp]; dup {
			t.Errorf("%s and %s share a fingerprint; they are distinct failures", name, prev)
		}
		seen[fp] = name
	}

	// Both stale-ref phrasings mean the same thing to an agent (re-snapshot),
	// so they must aggregate rather than split the ledger.
	notRecoverable := Fingerprint(`element ref "e4" not recoverable: no_key`)
	notFound := Fingerprint("ref not found — the page likely changed; re-run brw_snapshot to get current refs")
	if notRecoverable != notFound {
		t.Errorf("stale-ref phrasings fingerprint differently: %q vs %q", notRecoverable, notFound)
	}

	// The privacy guarantee still holds: caller-controlled text must not vary
	// the fingerprint of a recognised shape.
	a := Fingerprint(`element ref "SENTINEL_A" not recoverable: no_key`)
	b := Fingerprint(`element ref "SENTINEL_B" not recoverable: no_key`)
	if a != b {
		t.Errorf("fingerprint varies with caller-controlled ref: %q != %q", a, b)
	}

	// Every phrasing of "brw pointed at a tab it cannot drive" must aggregate, so
	// the ledger counts the outage once instead of splitting it three ways.
	drivable := []string{
		`extension bridge: Cannot access a chrome-extension:// URL of different extension`,
		`extension bridge: Cannot access contents of the page`,
		`extension bridge: Cannot attach to this target`,
		`no drivable tab: the active tab is chrome://settings/, which Chrome does not allow brw to control. Switch to a normal page tab, or pass an explicit tab_id.`,
	}
	want := Fingerprint(drivable[0])
	for _, msg := range drivable[1:] {
		if got := Fingerprint(msg); got != want {
			t.Errorf("not-drivable phrasing fingerprints differently: %q -> %q, want %q", msg, got, want)
		}
	}
}

func TestClassifyErrorSeparatesNonDrivableTabsFromToolFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			// The Bitwarden foreground-hijack failure. It must NOT be "tool" (an
			// agent mistake) and must NOT be "transport" (a bridge fault) — it is a
			// tab brw was never allowed to drive.
			name: "foreign extension page",
			err:  errors.New("extension bridge: Cannot access a chrome-extension:// URL of different extension"),
			want: "tab_not_drivable",
		},
		{
			name: "no drivable tab surfaced by the extension",
			err:  errors.New("no drivable tab: the active tab is chrome://settings/, which Chrome does not allow brw to control."),
			want: "tab_not_drivable",
		},
		{
			name: "devtools owns the debugger session",
			err:  errors.New("cannot control tab 42: another debugger (likely DevTools) is already attached"),
			want: "debugger_conflict",
		},
		{
			name: "a genuine tool failure still classifies as tool",
			err:  errors.New("click text: no visible element found for text \"Sign in\""),
			want: "tool",
		},
		{
			name: "recipe transport lacks document identity",
			err:  errors.New("deterministic recipe artifact capture requires main-document identity support"),
			want: "capability",
		},
		{
			name: "recipe document identity unavailable",
			err:  errors.New("could not verify the main document for recipe artifact capture"),
			want: "document_identity_unavailable",
		},
		{
			name: "recipe crossed document boundary",
			err:  errors.New("recipe artifact capture crossed a main-document boundary"),
			want: "document_changed",
		},
		{
			name: "a real transport drop is unaffected",
			err:  errors.New("extension bridge is not connected; load/click the Chrome extension first"),
			want: "transport",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
			// Neither new class is retryable: retrying re-fails until the human
			// closes the popout or DevTools, so retrying only burns the deadline.
			if tc.want == "tab_not_drivable" || tc.want == "debugger_conflict" {
				if Retryable(tc.want) {
					t.Errorf("Retryable(%q) = true, want false", tc.want)
				}
			}
		})
	}
}
