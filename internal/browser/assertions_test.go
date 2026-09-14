package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

const assertionFixture = `<!doctype html><html><head><title>Assertion fixture</title></head><body>
<h1 id="heading">Hello</h1>
<input id="name" value="Ada" data-kind="person" aria-label="Full name">
<input id="frozen" value="fixed" readonly aria-label="Frozen field">
<input id="agree" type="checkbox" checked aria-label="Agree to terms">
<button id="go" disabled>Go</button>
<button id="other">Other</button>
<p class="row">one</p><p class="row">two</p><p class="row">three</p>
<script>document.getElementById('name').focus();</script>
</body></html>`

func assertionFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, assertionFixture)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<html><head><title>Gone</title></head><body>gone</body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func int64Ptr(value int64) *int64 { return &value }

// TestAssertionsAgainstRealChrome drives every assertion kind through the real
// getters script in a real page, in both its passing and its failing form. The
// failing cases assert on the message, because a failure that does not name
// expected against actual costs the caller another round trip to find out what
// the page actually said.
func TestAssertionsAgainstRealChrome(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := assertionFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	snap, err := manager.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeHidden: true, Limit: 200})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	goRef := ""
	buttons := 0
	for _, element := range snap.Elements {
		if element.Role == "button" {
			buttons++
		}
		if element.Name == "Go" {
			goRef = element.Ref
		}
	}
	if goRef == "" {
		t.Fatalf("fixture button has no ref in %+v", snap.Elements)
	}
	// Guarding this keeps the semantic-count cases below honest: a count of zero
	// would satisfy "exactly 0" and prove nothing about the Find path.
	if buttons == 0 {
		t.Fatalf("snapshot found no button role in %+v", snap.Elements)
	}
	page := srv.URL + "/page"

	tests := []struct {
		name    string
		req     AssertRequest
		wantErr string
	}{
		{name: "url exact", req: AssertRequest{Assertion: AssertionURL, Expected: page}},
		{
			name:    "url exact mismatch names both sides",
			req:     AssertRequest{Assertion: AssertionURL, Expected: page + "/elsewhere"},
			wantErr: `url assertion failed: expected exact "` + page + `/elsewhere", actual "` + page + `"`,
		},
		{name: "url prefix", req: AssertRequest{Assertion: AssertionURL, Mode: AssertModePrefix, Expected: srv.URL}},
		{
			name:    "url prefix mismatch",
			req:     AssertRequest{Assertion: AssertionURL, Mode: AssertModePrefix, Expected: "https://example.invalid"},
			wantErr: `url assertion failed: expected prefix "https://example.invalid", actual "` + page + `"`,
		},
		{name: "url regex", req: AssertRequest{Assertion: AssertionURL, Mode: AssertModeRegex, Expected: `http://127\.0\.0\.1:\d+/page`}},
		{
			// A regex is anchored to the whole URL, so a pattern that merely
			// appears in it does not pass. This is the difference between
			// "starts with http" and "is this page".
			name:    "url regex is anchored to the whole url",
			req:     AssertRequest{Assertion: AssertionURL, Mode: AssertModeRegex, Expected: "http"},
			wantErr: `url assertion failed: expected regex "http", actual "` + page + `"`,
		},
		{name: "http status", req: AssertRequest{Assertion: AssertionHTTPStatus, Status: 200}},
		{
			name:    "http status mismatch",
			req:     AssertRequest{Assertion: AssertionHTTPStatus, Status: 404},
			wantErr: "http status assertion failed: expected 404, actual 200",
		},
		{name: "element count exact", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Count: intPtr(3)}},
		{
			name:    "element count exact mismatch",
			req:     AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Count: intPtr(4)},
			wantErr: `element count assertion failed: expected exactly 4 elements matching ".row", actual 3`,
		},
		{name: "element count range", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Min: intPtr(2), Max: intPtr(5)}},
		{
			name:    "element count below min",
			req:     AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Min: intPtr(5)},
			wantErr: `element count assertion failed: expected at least 5 elements matching ".row", actual 3`,
		},
		{
			name:    "element count above max",
			req:     AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Max: intPtr(2)},
			wantErr: `element count assertion failed: expected at most 2 elements matching ".row", actual 3`,
		},
		{name: "element count by semantic role", req: AssertRequest{Assertion: AssertionElementCount, Role: "button", Count: intPtr(buttons)}},
		{
			name:    "element count by semantic role mismatch",
			req:     AssertRequest{Assertion: AssertionElementCount, Role: "button", Count: intPtr(buttons + 1)},
			wantErr: fmt.Sprintf(`element count assertion failed: expected exactly %d elements matching role "button", actual %d`, buttons+1, buttons),
		},
		{name: "element state enabled", req: AssertRequest{Assertion: AssertionElementState, Ref: "#name", State: AssertStateEnabled}},
		{
			name:    "element state enabled on a disabled control",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#go", State: AssertStateEnabled},
			wantErr: `element state assertion failed: expected ref "#go" to be enabled, actual not enabled`,
		},
		{name: "element state negated on a snapshot ref", req: AssertRequest{Assertion: AssertionElementState, Ref: goRef, State: AssertStateEnabled, Negate: true}},
		{
			name:    "element state negation fails on an enabled control",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#other", State: AssertStateEnabled, Negate: true},
			wantErr: `element state assertion failed: expected ref "#other" to be not enabled, actual enabled`,
		},
		{name: "element state checked", req: AssertRequest{Assertion: AssertionElementState, Ref: "#agree", State: AssertStateChecked}},
		{
			name:    "element state checked on a text field",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#name", State: AssertStateChecked},
			wantErr: `element state assertion failed: expected ref "#name" to be checked, actual not checked`,
		},
		{name: "element state editable", req: AssertRequest{Assertion: AssertionElementState, Ref: "#name", State: AssertStateEditable}},
		{
			name:    "element state editable on a readonly field",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#frozen", State: AssertStateEditable},
			wantErr: `element state assertion failed: expected ref "#frozen" to be editable, actual not editable`,
		},
		{name: "element state focused", req: AssertRequest{Assertion: AssertionElementState, Ref: "#name", State: AssertStateFocused}},
		{
			name:    "element state focused on an unfocused control",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#other", State: AssertStateFocused},
			wantErr: `element state assertion failed: expected ref "#other" to be focused, actual not focused`,
		},
		{
			// "disabled" and "not on the page at all" are different bugs, and the
			// message has to say which one happened.
			name:    "element state on a missing element says so",
			req:     AssertRequest{Assertion: AssertionElementState, Ref: "#nope", State: AssertStateEnabled},
			wantErr: `element state assertion failed: expected ref "#nope" to be enabled, actual no element matched "#nope"`,
		},
		{name: "attribute exact", req: AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-kind", Expected: "person"}},
		{
			name:    "attribute exact mismatch",
			req:     AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-kind", Expected: "robot"},
			wantErr: `attribute assertion failed: expected attribute "data-kind" on ref "#name" to equal "robot", actual "person"`,
		},
		{name: "attribute contains", req: AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-kind", Mode: AssertModeContains, Expected: "pers"}},
		{
			name:    "attribute contains mismatch",
			req:     AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-kind", Mode: AssertModeContains, Expected: "zzz"},
			wantErr: `attribute assertion failed: expected attribute "data-kind" on ref "#name" to contain "zzz", actual "person"`,
		},
		{
			name:    "attribute absent is not the empty string",
			req:     AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-absent", Expected: ""},
			wantErr: `attribute assertion failed: expected attribute "data-absent" on ref "#name" to equal "", actual attribute is absent`,
		},
		{
			name:    "attribute on a missing element says so",
			req:     AssertRequest{Assertion: AssertionAttribute, Ref: "#nope", Attribute: "data-kind", Expected: "person"},
			wantErr: `attribute assertion failed: expected attribute "data-kind" on ref "#nope" to equal "person", actual no element matched "#nope"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Assert(ctx, manager, tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Assert(%+v) = %v, want pass", tt.req, err)
				}
				if !result.OK {
					t.Fatalf("Assert(%+v) returned %+v with no error", tt.req, result)
				}
				return
			}
			if err == nil {
				t.Fatalf("Assert(%+v) passed, want failure %q", tt.req, tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("message = %q, want %q", err.Error(), tt.wantErr)
			}
			if result.OK {
				t.Fatalf("failed assertion reported ok=true: %+v", result)
			}
		})
	}
}

// TestURLAssertionFragmentIsOptional pins the default: a "#fragment" is client
// state a router rewrites without navigating, so it is compared only on request.
func TestURLAssertionFragmentIsOptional(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := assertionFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	page := srv.URL + "/page"
	if _, err := manager.Open(ctx, page+"#section"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	if _, err := Assert(ctx, manager, AssertRequest{Assertion: AssertionURL, Expected: page}); err != nil {
		t.Fatalf("fragment should be ignored by default: %v", err)
	}
	_, err := Assert(ctx, manager, AssertRequest{Assertion: AssertionURL, Expected: page, IncludeFragment: true})
	if err == nil {
		t.Fatal("include_fragment did not compare the fragment")
	}
	want := `url assertion failed: expected exact "` + page + `", actual "` + page + `#section"`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if _, err := Assert(ctx, manager, AssertRequest{Assertion: AssertionURL, Expected: page + "#section", IncludeFragment: true}); err != nil {
		t.Fatalf("matching fragment should pass: %v", err)
	}
}

// TestHTTPStatusAssertionReadsTheDocumentNavigation proves the status comes from
// the main document's own navigation rather than from any subresource.
func TestHTTPStatusAssertionReadsTheDocumentNavigation(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := assertionFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/missing"); err != nil {
		t.Fatalf("open 404 fixture: %v", err)
	}
	if _, err := Assert(ctx, manager, AssertRequest{Assertion: AssertionHTTPStatus, Status: 404}); err != nil {
		t.Fatalf("404 page: %v", err)
	}
	_, err := Assert(ctx, manager, AssertRequest{Assertion: AssertionHTTPStatus, Status: 200})
	if err == nil {
		t.Fatal("404 page satisfied a 200 assertion")
	}
	if want := "http status assertion failed: expected 200, actual 404"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

// TestBatchRunsAssertSteps proves the assertion vocabulary is reachable from a
// brw_batch step and that a failing one stops the batch with the same message.
func TestBatchRunsAssertSteps(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := assertionFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/page"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	passing, err := manager.ExecuteBatch(ctx, []BatchStep{
		{Action: "assert", Assertion: &AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Count: intPtr(3)}},
		{Action: "assert", Assertion: &AssertRequest{Assertion: AssertionAttribute, Ref: "#name", Attribute: "data-kind", Expected: "person"}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !passing.OK || passing.StepsCompleted != 2 {
		t.Fatalf("batch = %+v, want both assert steps to pass", passing)
	}

	failing, err := manager.ExecuteBatch(ctx, []BatchStep{
		{Action: "assert", Assertion: &AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Count: intPtr(9)}},
		{Action: "assert", Assertion: &AssertRequest{Assertion: AssertionElementCount, Selector: ".row", Count: intPtr(3)}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	want := `element count assertion failed: expected exactly 9 elements matching ".row", actual 3`
	if failing.OK || failing.Error != want {
		t.Fatalf("batch = %+v, want it to stop with %q", failing, want)
	}
	if failing.StepsCompleted != 1 {
		t.Fatalf("steps_completed = %d, want the batch to stop at the failing assertion", failing.StepsCompleted)
	}
}

// TestDownloadDigestAssertion drives the digest check against the download
// ledger brw_downloads already keeps, with a file on disk rather than a stub
// hash, so a change to how the entry is located or read fails the test.
func TestDownloadDigestAssertion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "statement.csv")
	payload := []byte("month,total\n2026-01,42\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	size := int64(len(payload))

	manager := &Manager{
		downloadIndex: map[string]int{}, downloadVersions: map[string]uint64{},
		downloadCursors: map[string]uint64{}, downloadDir: dir, downloadsEnabled: true,
		downloads: []DownloadEntry{{
			GUID: "download-guid-1", URL: "https://example.test/statement.csv",
			SuggestedFilename: "statement.csv", State: "completed",
			ReceivedBytes: size, TotalBytes: size, Path: path,
		}},
	}

	tests := []struct {
		name    string
		req     AssertRequest
		wantErr string
	}{
		{name: "digest and size by filename", req: AssertRequest{Assertion: AssertionDownload, Filename: "statement.csv", SHA256: digest, Bytes: int64Ptr(size)}},
		{name: "digest by guid", req: AssertRequest{Assertion: AssertionDownload, DownloadGUID: "download-guid-1", SHA256: digest}},
		{name: "size alone", req: AssertRequest{Assertion: AssertionDownload, Filename: "statement.csv", Bytes: int64Ptr(size)}},
		{
			name: "wrong digest",
			req:  AssertRequest{Assertion: AssertionDownload, Filename: "statement.csv", SHA256: strings.Repeat("ab", 32)},
			wantErr: fmt.Sprintf(`download digest assertion failed: expected download "statement.csv" to have sha256 %q, actual sha256 %q, %d bytes`,
				strings.Repeat("ab", 32), digest, size),
		},
		{
			name: "wrong size",
			req:  AssertRequest{Assertion: AssertionDownload, Filename: "statement.csv", Bytes: int64Ptr(size + 1)},
			wantErr: fmt.Sprintf(`download digest assertion failed: expected download "statement.csv" to have %d bytes, actual sha256 %q, %d bytes`,
				size+1, digest, size),
		},
		{
			name:    "unknown download",
			req:     AssertRequest{Assertion: AssertionDownload, Filename: "other.csv", SHA256: digest},
			wantErr: fmt.Sprintf(`download digest assertion failed: expected download "other.csv" to have sha256 %q, actual no download matched "other.csv"`, digest),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Assert(context.Background(), manager, tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Assert(%+v) = %v, want pass", tt.req, err)
				}
				if !result.OK {
					t.Fatalf("Assert(%+v) returned %+v with no error", tt.req, result)
				}
				return
			}
			if err == nil {
				t.Fatalf("Assert(%+v) passed, want failure", tt.req)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("message = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestDownloadDigestAssertionNamesAnUnsupportedTransport pins the capability
// error: a transport that cannot see downloads must say so by name rather than
// quietly report "no download matched".
func TestDownloadDigestAssertionNamesAnUnsupportedTransport(t *testing.T) {
	src := &stubAssertSource{downloads: DownloadsResult{Supported: false, Note: "the extension bridge cannot observe downloads"}}
	_, err := evaluateAssertion(context.Background(), src, AssertRequest{
		Assertion: AssertionDownload, Filename: "statement.csv", Bytes: int64Ptr(1),
	})
	if err == nil {
		t.Fatal("unsupported transport passed the assertion")
	}
	want := "download digest assertions are unavailable on this transport: the extension bridge cannot observe downloads"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

// stubAssertSource records the expressions an assertion evaluates so a test can
// prove which script it went through.
type stubAssertSource struct {
	expressions []string
	values      map[string]any
	downloads   DownloadsResult
	find        snapshot.FindResult
}

func (s *stubAssertSource) Evaluate(_ context.Context, expression string) (any, error) {
	s.expressions = append(s.expressions, expression)
	if value, ok := s.values[expression]; ok {
		return value, nil
	}
	return map[string]any{"value": nil}, nil
}

func (s *stubAssertSource) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return s.find, nil
}

func (s *stubAssertSource) Downloads(context.Context) (DownloadsResult, error) {
	return s.downloads, nil
}

// TestElementAssertionsReadThroughTheGettersScript pins the reuse: element state
// and attribute assertions must evaluate the shared getters script, not a fresh
// per-call snippet with its own element-resolution rules.
func TestElementAssertionsReadThroughTheGettersScript(t *testing.T) {
	stateExpr := snapshot.BuildGetExpression("state", "e7", "")
	attrExpr := snapshot.BuildGetExpression("attr", "e7", "aria-expanded")

	src := &stubAssertSource{values: map[string]any{
		stateExpr: map[string]any{"value": map[string]any{"found": true, "enabled": true}},
		attrExpr:  map[string]any{"value": "true"},
	}}
	if _, err := evaluateAssertion(context.Background(), src, AssertRequest{
		Assertion: AssertionElementState, Ref: "e7", State: AssertStateEnabled,
	}); err != nil {
		t.Fatalf("element state: %v", err)
	}
	if _, err := evaluateAssertion(context.Background(), src, AssertRequest{
		Assertion: AssertionAttribute, Ref: "e7", Attribute: "aria-expanded", Expected: "true",
	}); err != nil {
		t.Fatalf("attribute: %v", err)
	}
	for _, want := range []string{stateExpr, attrExpr} {
		found := false
		for _, got := range src.expressions {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("assertion did not evaluate the getters expression %.60s...", want)
		}
	}
	for _, got := range src.expressions {
		if !strings.HasPrefix(got, snapshot.GetScript) {
			t.Fatalf("assertion injected a script that is not snapshot.GetScript: %.120s", got)
		}
	}
}

func TestAssertRequestValidation(t *testing.T) {
	tests := []struct {
		name string
		req  AssertRequest
		want string
	}{
		{name: "missing kind", req: AssertRequest{}, want: "assertion is required"},
		{name: "unknown kind", req: AssertRequest{Assertion: "vibes"}, want: `unknown assertion "vibes"`},
		{name: "url without expected", req: AssertRequest{Assertion: AssertionURL}, want: "url assertion requires expected"},
		{name: "url bad mode", req: AssertRequest{Assertion: AssertionURL, Expected: "x", Mode: "fuzzy"}, want: "url assertion mode must be exact, prefix or regex"},
		{name: "url bad regex", req: AssertRequest{Assertion: AssertionURL, Expected: "([", Mode: AssertModeRegex}, want: "url assertion regex is invalid"},
		{name: "status out of range", req: AssertRequest{Assertion: AssertionHTTPStatus, Status: 42}, want: "http status assertion requires status between 100 and 599"},
		{name: "count with neither selector nor role", req: AssertRequest{Assertion: AssertionElementCount, Count: intPtr(1)}, want: "element count assertion requires exactly one of selector or role"},
		{name: "count with both selector and role", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".x", Role: "button", Count: intPtr(1)}, want: "element count assertion requires exactly one of selector or role"},
		{name: "count without a bound", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".x"}, want: "element count assertion requires count, min or max"},
		{name: "count with both exact and range", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".x", Count: intPtr(1), Min: intPtr(1)}, want: "element count assertion takes count, or min/max, not both"},
		{name: "count with min above max", req: AssertRequest{Assertion: AssertionElementCount, Selector: ".x", Min: intPtr(4), Max: intPtr(2)}, want: "element count assertion min must not exceed max"},
		{name: "state without ref", req: AssertRequest{Assertion: AssertionElementState, State: AssertStateEnabled}, want: "element state assertion requires ref"},
		{name: "unknown state", req: AssertRequest{Assertion: AssertionElementState, Ref: "e1", State: "sleepy"}, want: "element state assertion state must be enabled, editable, checked or focused"},
		{name: "attribute without name", req: AssertRequest{Assertion: AssertionAttribute, Ref: "e1"}, want: "attribute assertion requires attribute"},
		{name: "download with both selectors", req: AssertRequest{Assertion: AssertionDownload, DownloadGUID: "g", Filename: "f", Bytes: int64Ptr(1)}, want: "download assertion requires exactly one of download_guid or filename"},
		{name: "download without an expectation", req: AssertRequest{Assertion: AssertionDownload, Filename: "f"}, want: "download assertion requires sha256 or bytes"},
		{name: "download with a short digest", req: AssertRequest{Assertion: AssertionDownload, Filename: "f", SHA256: "abc"}, want: "download assertion sha256 must be 64 hex characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAssertRequest(tt.req)
			if err == nil {
				t.Fatalf("ValidateAssertRequest(%+v) = nil, want %q", tt.req, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}
