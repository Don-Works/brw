package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

// Assertion kinds. These are deliberately one discriminated request rather than
// one tool per question: the six share their validation, their expected-vs-actual
// failure text, and their no-retry contract, and an agent picks a kind out of an
// enum instead of learning six more tool names.
//
// None of them retries. An assertion that only passes on the second read is
// describing a wait, and the wait belongs in brw_wait_for where the caller can
// see and bound it; hiding it inside an assertion turns a race into a silent
// sleep that nobody can find later.
const (
	AssertionURL          = "url"
	AssertionHTTPStatus   = "http_status"
	AssertionElementCount = "element_count"
	AssertionElementState = "element_state"
	AssertionAttribute    = "attribute"
	AssertionDownload     = "download"
)

// Comparison modes. exact/prefix/regex apply to url; exact/contains to attribute.
const (
	AssertModeExact    = "exact"
	AssertModePrefix   = "prefix"
	AssertModeRegex    = "regex"
	AssertModeContains = "contains"
)

// Element states readable by AssertionElementState.
const (
	AssertStateEnabled  = "enabled"
	AssertStateEditable = "editable"
	AssertStateChecked  = "checked"
	AssertStateFocused  = "focused"
)

// AssertRequest is one deterministic check. Only the fields its Assertion names
// are read; the rest must be left unset, and a request that sets a field for a
// different kind is rejected rather than silently ignored.
type AssertRequest struct {
	Assertion string `json:"assertion"`

	// url and attribute.
	Expected string `json:"expected,omitempty"`
	Mode     string `json:"mode,omitempty"`
	// IncludeFragment compares the "#..." tail of a URL. Off by default: a
	// fragment is client state that a router rewrites without navigating, so
	// comparing it by default makes ordinary URL assertions flaky.
	IncludeFragment bool `json:"include_fragment,omitempty"`

	// http_status.
	Status int `json:"status,omitempty"`

	// element_count: a CSS selector, or a semantic role (with an optional exact
	// accessible name) resolved the same way brw_find resolves one.
	Selector string `json:"selector,omitempty"`
	Role     string `json:"role,omitempty"`
	Name     string `json:"name,omitempty"`
	Count    *int   `json:"count,omitempty"`
	Min      *int   `json:"min,omitempty"`
	Max      *int   `json:"max,omitempty"`

	// element_state and attribute.
	Ref    string `json:"ref,omitempty"`
	State  string `json:"state,omitempty"`
	Negate bool   `json:"negate,omitempty"`

	// attribute.
	Attribute string `json:"attribute,omitempty"`

	// download.
	DownloadGUID string `json:"download_guid,omitempty"`
	Filename     string `json:"filename,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	Bytes        *int64 `json:"bytes,omitempty"`
}

// AssertResult reports the comparison in machine-readable form. A failure
// returns a populated result AND an error, so a caller that renders the result
// and a caller that only checks err both see expected against actual.
type AssertResult struct {
	OK        bool   `json:"ok"`
	Assertion string `json:"assertion"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
}

// AssertSource is the read surface an assertion needs. Every transport already
// satisfies it, so the evaluator is written once instead of per transport.
type AssertSource interface {
	Evaluate(context.Context, string) (any, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Downloads(context.Context) (DownloadsResult, error)
}

// Asserter is an optional transport capability: evaluate the assertion where
// the browser actually is. The upstream HTTP proxy implements it because a
// download digest hashes a file that exists only on the browser host, and
// re-deriving it across the proxy would hash nothing.
type Asserter interface {
	Assert(context.Context, AssertRequest) (AssertResult, error)
}

// maxSemanticCountMatches bounds the semantic search behind an element_count
// assertion. A count that hits the cap is reported as an error rather than as a
// number, because a truncated search answers a different question than the one
// the caller asked.
const maxSemanticCountMatches = 1000

// Assert runs one assertion against a controller, preferring the controller's
// own Asserter capability when it has one.
func Assert(ctx context.Context, controller Controller, req AssertRequest) (AssertResult, error) {
	// element_state and attribute assertions carry a ref, and every transport
	// routes through here, so the cross-origin refusal belongs at this one point
	// rather than in each transport's Asserter.
	if err := GuardCrossOriginRefs("assert "+req.Assertion, GenericCrossOriginRemedy, req.Ref); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	if remote, ok := controller.(Asserter); ok {
		return remote.Assert(ctx, req)
	}
	return evaluateAssertion(ctx, controller, req)
}

func evaluateAssertion(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	if err := ValidateAssertRequest(req); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	switch req.Assertion {
	case AssertionURL:
		return assertURL(ctx, src, req)
	case AssertionHTTPStatus:
		return assertHTTPStatus(ctx, src, req)
	case AssertionElementCount:
		return assertElementCount(ctx, src, req)
	case AssertionElementState:
		return assertElementState(ctx, src, req)
	case AssertionAttribute:
		return assertAttribute(ctx, src, req)
	case AssertionDownload:
		return assertDownload(ctx, src, req)
	default:
		return AssertResult{Assertion: req.Assertion}, fmt.Errorf("unknown assertion %q", req.Assertion)
	}
}

// ValidateAssertRequest rejects a request whose fields do not belong to its
// kind. Ignoring a stray field would let a caller believe it had asserted
// something it had not.
func ValidateAssertRequest(req AssertRequest) error {
	switch req.Assertion {
	case AssertionURL:
		if strings.TrimSpace(req.Expected) == "" {
			return errors.New("url assertion requires expected")
		}
		switch req.Mode {
		case "", AssertModeExact, AssertModePrefix, AssertModeRegex:
		default:
			return fmt.Errorf("url assertion mode must be exact, prefix or regex, got %q", req.Mode)
		}
		if req.Mode == AssertModeRegex {
			if _, err := compileAnchored(req.Expected); err != nil {
				return fmt.Errorf("url assertion regex is invalid: %w", err)
			}
		}
	case AssertionHTTPStatus:
		if req.Status < 100 || req.Status > 599 {
			return errors.New("http status assertion requires status between 100 and 599")
		}
	case AssertionElementCount:
		if (strings.TrimSpace(req.Selector) == "") == (strings.TrimSpace(req.Role) == "") {
			return errors.New("element count assertion requires exactly one of selector or role")
		}
		if req.Count == nil && req.Min == nil && req.Max == nil {
			return errors.New("element count assertion requires count, min or max")
		}
		if req.Count != nil && (req.Min != nil || req.Max != nil) {
			return errors.New("element count assertion takes count, or min/max, not both")
		}
		for name, bound := range map[string]*int{"count": req.Count, "min": req.Min, "max": req.Max} {
			if bound != nil && *bound < 0 {
				return fmt.Errorf("element count assertion %s must not be negative", name)
			}
		}
		if req.Min != nil && req.Max != nil && *req.Min > *req.Max {
			return errors.New("element count assertion min must not exceed max")
		}
	case AssertionElementState:
		if strings.TrimSpace(req.Ref) == "" {
			return errors.New("element state assertion requires ref")
		}
		switch req.State {
		case AssertStateEnabled, AssertStateEditable, AssertStateChecked, AssertStateFocused:
		default:
			return fmt.Errorf("element state assertion state must be enabled, editable, checked or focused, got %q", req.State)
		}
	case AssertionAttribute:
		if strings.TrimSpace(req.Ref) == "" {
			return errors.New("attribute assertion requires ref")
		}
		if strings.TrimSpace(req.Attribute) == "" {
			return errors.New("attribute assertion requires attribute")
		}
		switch req.Mode {
		case "", AssertModeExact, AssertModeContains:
		default:
			return fmt.Errorf("attribute assertion mode must be exact or contains, got %q", req.Mode)
		}
	case AssertionDownload:
		if (strings.TrimSpace(req.DownloadGUID) == "") == (strings.TrimSpace(req.Filename) == "") {
			return errors.New("download assertion requires exactly one of download_guid or filename")
		}
		if strings.TrimSpace(req.SHA256) == "" && req.Bytes == nil {
			return errors.New("download assertion requires sha256 or bytes")
		}
		if digest := strings.TrimSpace(req.SHA256); digest != "" {
			if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
				return errors.New("download assertion sha256 must be 64 hex characters")
			}
		}
		if req.Bytes != nil && *req.Bytes < 0 {
			return errors.New("download assertion bytes must not be negative")
		}
	case "":
		return errors.New("assertion is required")
	default:
		return fmt.Errorf("unknown assertion %q: use url, http_status, element_count, element_state, attribute or download", req.Assertion)
	}
	return nil
}

func assertURL(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	var actual string
	if err := probeGetter(ctx, src, "url", "", "", &actual); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	mode := req.Mode
	if mode == "" {
		mode = AssertModeExact
	}
	compared, expected := actual, req.Expected
	if !req.IncludeFragment {
		compared = trimFragment(compared)
		// A regex is not a URL. Its '#' can open an optional fragment group or
		// sit inside an alternation branch, so cutting the pattern there either
		// stops it compiling — after validation compiled the whole pattern and
		// accepted it — or silently changes which URLs it matches.
		if mode != AssertModeRegex {
			expected = trimFragment(expected)
		}
	}
	matched := false
	switch mode {
	case AssertModeExact:
		matched = compared == expected
	case AssertModePrefix:
		matched = strings.HasPrefix(compared, expected)
	case AssertModeRegex:
		pattern, err := compileAnchored(expected)
		if err != nil {
			return AssertResult{Assertion: req.Assertion}, fmt.Errorf("url assertion regex is invalid: %w", err)
		}
		matched = pattern.MatchString(compared)
	}
	if !matched {
		return assertionFailed(req.Assertion, fmt.Sprintf("%s %s", mode, quote(expected)), quote(compared))
	}
	return assertionPassed(req.Assertion, fmt.Sprintf("%s %s", mode, quote(expected)), quote(compared)), nil
}

func assertHTTPStatus(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	var actual int
	if err := probeGetter(ctx, src, "status", "", "", &actual); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	if actual == 0 {
		return AssertResult{Assertion: req.Assertion, Expected: strconv.Itoa(req.Status)},
			errors.New("http status assertion is unavailable here: the current document reports no navigation status, which is what a data:, blob: or about: document looks like")
	}
	if actual != req.Status {
		return assertionFailed(req.Assertion, strconv.Itoa(req.Status), strconv.Itoa(actual))
	}
	return assertionPassed(req.Assertion, strconv.Itoa(req.Status), strconv.Itoa(actual)), nil
}

func assertElementCount(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	actual, query, err := countMatches(ctx, src, req)
	if err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	return AssertCount(actual, query, req)
}

// AssertCount checks an already-counted set of matches against the request's
// count/min/max. It is exported so a caller that resolves matches its own way —
// the recipe surface resolves a semantic Target with its own filters — produces
// the identical failure text instead of a second dialect of it. description
// names what was counted and appears verbatim in that text.
func AssertCount(actual int, description string, req AssertRequest) (AssertResult, error) {
	expected := describeCountBound(req) + " matching " + description
	within := true
	switch {
	case req.Count != nil:
		within = actual == *req.Count
	default:
		if req.Min != nil && actual < *req.Min {
			within = false
		}
		if req.Max != nil && actual > *req.Max {
			within = false
		}
	}
	if !within {
		return assertionFailed(AssertionElementCount, expected, strconv.Itoa(actual))
	}
	return assertionPassed(AssertionElementCount, expected, strconv.Itoa(actual)), nil
}

func countMatches(ctx context.Context, src AssertSource, req AssertRequest) (int, string, error) {
	if selector := strings.TrimSpace(req.Selector); selector != "" {
		var count int
		if err := probeGetter(ctx, src, "count", selector, "", &count); err != nil {
			return 0, "", err
		}
		return count, quote(selector), nil
	}
	query := req.Name
	result, err := src.Find(ctx, snapshot.FindOptions{
		Query: query, Role: req.Role, Limit: maxSemanticCountMatches, ViewportOnly: false, IncludeHidden: true,
	})
	if err != nil {
		return 0, "", err
	}
	if truncated, _ := result.Metadata["truncated"].(bool); truncated {
		return 0, "", fmt.Errorf("element count assertion cannot count role %q: the semantic search was truncated at %d matches", req.Role, maxSemanticCountMatches)
	}
	count := 0
	for _, element := range result.Elements {
		if element.Role != req.Role {
			continue
		}
		if req.Name != "" && element.Name != req.Name {
			continue
		}
		count++
	}
	description := "role " + quote(req.Role)
	if req.Name != "" {
		description += " named " + quote(req.Name)
	}
	return count, description, nil
}

func describeCountBound(req AssertRequest) string {
	switch {
	case req.Count != nil:
		return fmt.Sprintf("exactly %d elements", *req.Count)
	case req.Min != nil && req.Max != nil:
		return fmt.Sprintf("between %d and %d elements", *req.Min, *req.Max)
	case req.Min != nil:
		return fmt.Sprintf("at least %d elements", *req.Min)
	case req.Max != nil:
		return fmt.Sprintf("at most %d elements", *req.Max)
	default:
		return "any number of elements"
	}
}

// elementState mirrors the object the getters script's 'state' case returns.
type elementState struct {
	Found    bool `json:"found"`
	Visible  bool `json:"visible"`
	Enabled  bool `json:"enabled"`
	Editable bool `json:"editable"`
	Checked  bool `json:"checked"`
	Focused  bool `json:"focused"`
}

func assertElementState(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	var state elementState
	if err := probeGetter(ctx, src, "state", req.Ref, "", &state); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	expected := fmt.Sprintf("ref %s to be %s", quote(req.Ref), describeWantedState(req))
	if !state.Found {
		return assertionFailed(req.Assertion, expected, "no element matched "+quote(req.Ref))
	}
	var actual bool
	switch req.State {
	case AssertStateEnabled:
		actual = state.Enabled
	case AssertStateEditable:
		actual = state.Editable
	case AssertStateChecked:
		actual = state.Checked
	case AssertStateFocused:
		actual = state.Focused
	}
	observed := req.State
	if !actual {
		observed = "not " + req.State
	}
	if actual == req.Negate {
		return assertionFailed(req.Assertion, expected, observed)
	}
	return assertionPassed(req.Assertion, expected, observed), nil
}

func describeWantedState(req AssertRequest) string {
	if req.Negate {
		return "not " + req.State
	}
	return req.State
}

func assertAttribute(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	// The element is resolved twice on purpose: the state probe distinguishes
	// "attribute absent" from "element absent", which the attribute read alone
	// cannot, and a failure message that confuses the two sends the caller
	// hunting for the wrong bug.
	var state elementState
	if err := probeGetter(ctx, src, "state", req.Ref, "", &state); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	mode := req.Mode
	if mode == "" {
		mode = AssertModeExact
	}
	verb := "equal"
	if mode == AssertModeContains {
		verb = "contain"
	}
	expected := fmt.Sprintf("attribute %s on ref %s to %s %s", quote(req.Attribute), quote(req.Ref), verb, quote(req.Expected))
	if !state.Found {
		return assertionFailed(req.Assertion, expected, "no element matched "+quote(req.Ref))
	}
	var actual *string
	if err := probeGetter(ctx, src, "attr", req.Ref, req.Attribute, &actual); err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	if actual == nil {
		return assertionFailed(req.Assertion, expected, "attribute is absent")
	}
	matched := *actual == req.Expected
	if mode == AssertModeContains {
		matched = strings.Contains(*actual, req.Expected)
	}
	if !matched {
		return assertionFailed(req.Assertion, expected, quote(*actual))
	}
	return assertionPassed(req.Assertion, expected, quote(*actual)), nil
}

func assertDownload(ctx context.Context, src AssertSource, req AssertRequest) (AssertResult, error) {
	result, err := src.Downloads(ctx)
	if err != nil {
		return AssertResult{Assertion: req.Assertion}, err
	}
	return AssertDownloadFrom(result, req)
}

// AssertDownloadFrom checks the named download against a downloads read the
// caller already performed. The recipe surface has to read the ledger itself —
// that read consumes the run's delta cursor, so it must keep the entries — and
// calls this so both paths produce one capability error and one failure text.
func AssertDownloadFrom(result DownloadsResult, req AssertRequest) (AssertResult, error) {
	if !result.Supported {
		note := strings.TrimSpace(result.Note)
		if note == "" {
			note = "this browser transport does not record downloads"
		}
		return AssertResult{Assertion: AssertionDownload}, fmt.Errorf("download digest assertions are unavailable on this transport: %s", note)
	}
	if !result.FilePaths {
		note := strings.TrimSpace(result.Note)
		if note == "" {
			note = "this browser transport reports no file path for a completed download"
		}
		return AssertResult{Assertion: AssertionDownload}, fmt.Errorf("download digest assertions are unavailable on this transport: %s", note)
	}
	entry, found := SelectDownloadEntry(result.Downloads, req.DownloadGUID, req.Filename)
	if !found {
		return assertionFailed(AssertionDownload, describeDownloadExpectation(req), "no download matched "+quote(downloadSelector(req)))
	}
	return AssertDownloadEntry(entry, req)
}

// SelectDownloadEntry picks the download a digest assertion names out of what
// brw_downloads recorded. The newest match wins: a filename can repeat across a
// session, and the one a flow just produced is the one being asserted.
func SelectDownloadEntry(entries []DownloadEntry, guid, filename string) (DownloadEntry, bool) {
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if guid != "" && entry.GUID != guid {
			continue
		}
		if filename != "" && entry.SuggestedFilename != filename {
			continue
		}
		return entry, true
	}
	return DownloadEntry{}, false
}

// AssertDownloadEntry checks one recorded download's bytes on disk against the
// expected sha256 and/or size. It hashes the file rather than trusting the
// progress events, because received_bytes counts what Chrome reported, not what
// landed on disk.
func AssertDownloadEntry(entry DownloadEntry, req AssertRequest) (AssertResult, error) {
	expected := describeDownloadExpectation(req)
	if entry.State != "completed" {
		return assertionFailed(AssertionDownload, expected, fmt.Sprintf("download %s is %s", quote(entry.SuggestedFilename), entry.State))
	}
	if strings.TrimSpace(entry.Path) == "" {
		return assertionFailed(AssertionDownload, expected, fmt.Sprintf("download %s has no file on disk", quote(entry.SuggestedFilename)))
	}
	digest, size, err := hashFile(entry.Path)
	if err != nil {
		return AssertResult{Assertion: AssertionDownload, Expected: expected}, fmt.Errorf("download digest assertion could not read the downloaded file: %w", err)
	}
	actual := fmt.Sprintf("sha256 %s, %d bytes", quote(digest), size)
	if want := strings.ToLower(strings.TrimSpace(req.SHA256)); want != "" && want != digest {
		return assertionFailed(AssertionDownload, expected, actual)
	}
	if req.Bytes != nil && *req.Bytes != size {
		return assertionFailed(AssertionDownload, expected, actual)
	}
	return assertionPassed(AssertionDownload, expected, actual), nil
}

func describeDownloadExpectation(req AssertRequest) string {
	parts := make([]string, 0, 2)
	if digest := strings.ToLower(strings.TrimSpace(req.SHA256)); digest != "" {
		parts = append(parts, "sha256 "+quote(digest))
	}
	if req.Bytes != nil {
		parts = append(parts, fmt.Sprintf("%d bytes", *req.Bytes))
	}
	return fmt.Sprintf("download %s to have %s", quote(downloadSelector(req)), strings.Join(parts, ", "))
}

func downloadSelector(req AssertRequest) string {
	if guid := strings.TrimSpace(req.DownloadGUID); guid != "" {
		return guid
	}
	return req.Filename
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// probeGetter reads one value through the shared getters script so element
// resolution — refs, same-origin frames, open shadow roots — behaves exactly as
// it does for brw_get instead of drifting into a per-assertion selector dialect.
//
// It carries brw_get's trace label too, because the label is what tells the
// takeover guard this is one of brw's own reads rather than a caller's
// expression. Without it an assertion is refused while a human holds the
// browser — one layer below the guard that classifies every assert step as
// read-only precisely so a hold does not stop the agent finding out what the
// human did. The label is also what puts "count #go" in the activity feed in
// place of the ~10 KB walker script.
func probeGetter(ctx context.Context, src AssertSource, what, target, name string, out any) error {
	label := snapshot.GetRequest{What: what, Target: target, Name: name}.TraceLabel()
	raw, err := src.Evaluate(WithTraceLabel(ctx, TraceActionGet, label), snapshot.BuildGetExpression(what, target, name))
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("page returned an unreadable %s value: %w", what, err)
	}
	var wrapper struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(encoded, &wrapper); err != nil || len(wrapper.Value) == 0 {
		return fmt.Errorf("page returned no %s value", what)
	}
	if err := json.Unmarshal(wrapper.Value, out); err != nil {
		return fmt.Errorf("page returned an unreadable %s value: %w", what, err)
	}
	return nil
}

// compileAnchored compiles a url regex that must match the whole URL. A
// half-anchored pattern is the classic way a URL assertion passes on a page it
// was never meant to accept.
func compileAnchored(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(`\A(?:` + pattern + `)\z`)
}

func trimFragment(raw string) string {
	if index := strings.Index(raw, "#"); index >= 0 {
		return raw[:index]
	}
	return raw
}

func quote(value string) string {
	return strconv.Quote(value)
}

func assertionPassed(kind, expected, actual string) AssertResult {
	return AssertResult{OK: true, Assertion: kind, Expected: expected, Actual: actual}
}

func assertionFailed(kind, expected, actual string) (AssertResult, error) {
	return AssertResult{Assertion: kind, Expected: expected, Actual: actual},
		fmt.Errorf("%s assertion failed: expected %s, actual %s", assertionLabel(kind), expected, actual)
}

func assertionLabel(kind string) string {
	switch kind {
	case AssertionHTTPStatus:
		return "http status"
	case AssertionElementCount:
		return "element count"
	case AssertionElementState:
		return "element state"
	case AssertionDownload:
		return "download digest"
	default:
		return kind
	}
}
