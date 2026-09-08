package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// A daemon that proxies or bridges cannot stream, and must say so rather than
// hold open an empty connection the caller will wait on forever.
func TestSessionStreamRefusesControllersThatCannotSubscribe(t *testing.T) {
	s := New("127.0.0.1:0", nonStreamingController{})
	rec := httptest.NewRecorder()
	s.sessionStream(rec, httptest.NewRequest(http.MethodGet, "/api/session/stream", nil))

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	if body := rec.Body.String(); body == "" {
		t.Fatal("a refusal must explain where to subscribe instead")
	}
}

type nonStreamingController struct{ browser.Controller }
