package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// The /api/page/cookies endpoint must carry every brw_cookies field across the
// HTTP boundary and return the controller's result JSON verbatim.
func TestCookiesEndpointForwardsParamsAndResult(t *testing.T) {
	ctrl := &fakeController{}
	server := New("", ctrl)
	body := bytes.NewBufferString(`{"action":"set","name":"sid","value":"v","url":"https://example.test/","path":"/","secure":true,"http_only":true,"same_site":"strict","expires":1893456000,"tab_id":"31"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/page/cookies", body)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(ctrl.cookiesParams) != 1 {
		t.Fatalf("controller saw %d cookie calls, want 1", len(ctrl.cookiesParams))
	}
	got := ctrl.cookiesParams[0]
	want := browser.CookieParams{
		Action: "set", Name: "sid", Value: "v", URL: "https://example.test/", Path: "/",
		Secure: true, HTTPOnly: true, SameSite: "strict", Expires: 1893456000,
	}
	if got != want {
		t.Fatalf("params = %+v, want %+v", got, want)
	}
	var result browser.CookieResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Count != 1 || len(result.Cookies) != 1 || !result.Cookies[0].HTTPOnly {
		t.Fatalf("result = %+v", result)
	}
}

// A controller-side transport limitation (the extension bridge refusing cookie
// access) must surface as a 400 with the reason intact, not a 5xx.
func TestCookiesEndpointSurfacesTransportLimitation(t *testing.T) {
	server := New("", &fakeController{})
	body := bytes.NewBufferString(`{"action":"list"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/page/cookies", body)
	rec := httptest.NewRecorder()
	// Swap in a refusing controller via the exported surface: easiest is a
	// dedicated server instance, so re-create with a stub type.
	refusing := &refusingCookiesController{}
	server2 := New("", refusing)
	rec2 := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req) // sanity: normal controller path
	req2 := httptest.NewRequest(http.MethodPost, "/api/page/cookies", bytes.NewBufferString(`{"action":"list"}`))
	server2.server.Handler.ServeHTTP(rec2, req2)
	if rec.Code != http.StatusOK {
		t.Fatalf("normal path status = %d", rec.Code)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("refusing path status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte("extension-bridge")) {
		t.Fatalf("error lost the transport reason: %s", rec2.Body.String())
	}
}

type refusingCookiesController struct {
	fakeController
}

func (r *refusingCookiesController) Cookies(_ context.Context, _ browser.CookieParams) (browser.CookieResult, error) {
	return browser.CookieResult{}, errors.New("cookie access is not supported on the extension-bridge transport")
}
