package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// TestControllerForwardsAssertionsUpstream pins the proxy contract: the whole
// request crosses to the browser host, and a failure comes back with its
// expected-against-actual text intact rather than as a bare transport error.
func TestControllerForwardsAssertionsUpstream(t *testing.T) {
	var seenPath string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("content-type", "application/json")
		if seenBody["assertion"] == "download" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":        false,
				"assertion": "download",
				"expected":  `download "export.csv" to have 10 bytes`,
				"actual":    `sha256 "abc", 12 bytes`,
				"error":     `download digest assertion failed: expected download "export.csv" to have 10 bytes, actual sha256 "abc", 12 bytes`,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(browser.AssertResult{OK: true, Assertion: "url", Expected: `exact "x"`, Actual: `"x"`})
	}))
	defer srv.Close()

	controller, err := New(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The upstream proxy must present itself as an Asserter, or a download digest
	// would be hashed on the wrong machine.
	if _, ok := any(controller).(browser.Asserter); !ok {
		t.Fatal("controller does not implement browser.Asserter")
	}

	ctx := browser.WithTabID(context.Background(), "tab-3")
	result, err := controller.Assert(ctx, browser.AssertRequest{Assertion: browser.AssertionURL, Expected: "x"})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if seenPath != "/api/page/assert" {
		t.Fatalf("path = %q, want /api/page/assert", seenPath)
	}
	if seenBody["tab_id"] != "tab-3" {
		t.Fatalf("body = %#v, want tab_id forwarded", seenBody)
	}

	bytes := int64(10)
	failed, err := controller.Assert(context.Background(), browser.AssertRequest{
		Assertion: browser.AssertionDownload, Filename: "export.csv", Bytes: &bytes,
	})
	if err == nil {
		t.Fatal("upstream failure did not surface as an error")
	}
	want := `download digest assertion failed: expected download "export.csv" to have 10 bytes, actual sha256 "abc", 12 bytes`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	// AssertResult promises a populated result alongside the error. A caller
	// that renders expected against actual must not have to parse the message
	// back out on this transport and not on the other one.
	if failed.OK || failed.Assertion != browser.AssertionDownload {
		t.Fatalf("failed result = %+v, want the refusal decoded", failed)
	}
	if failed.Expected != `download "export.csv" to have 10 bytes` || failed.Actual != `sha256 "abc", 12 bytes` {
		t.Fatalf("failed result = %+v, want expected against actual", failed)
	}
}

// TestControllerAssertSurvivesAnUpstreamFailureWithoutAResult keeps the decode
// best-effort: a refusal that carries only a message (a request rejected before
// it reached the evaluator) still returns that message, not a decode error.
func TestControllerAssertSurvivesAnUpstreamFailureWithoutAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"http status assertion requires status between 100 and 599"}`))
	}))
	defer srv.Close()

	controller, err := New(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Assert(context.Background(), browser.AssertRequest{
		Assertion: browser.AssertionHTTPStatus, Status: 42,
	})
	if err == nil || err.Error() != "http status assertion requires status between 100 and 599" {
		t.Fatalf("error = %v, want the upstream message", err)
	}
	if result.OK || result.Expected != "" || result.Actual != "" {
		t.Fatalf("result = %+v, want an empty result", result)
	}
}
