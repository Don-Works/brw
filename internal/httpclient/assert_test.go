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
				"error": `download digest assertion failed: expected download "export.csv" to have 10 bytes, actual sha256 "abc", 12 bytes`,
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
	_, err = controller.Assert(context.Background(), browser.AssertRequest{
		Assertion: browser.AssertionDownload, Filename: "export.csv", Bytes: &bytes,
	})
	if err == nil {
		t.Fatal("upstream failure did not surface as an error")
	}
	want := `download digest assertion failed: expected download "export.csv" to have 10 bytes, actual sha256 "abc", 12 bytes`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}
