package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func TestBearerGuard(t *testing.T) {
	bearer := strings.Repeat("k", 32)
	for _, tc := range []struct {
		name   string
		bearer string
		header []string
		want   int
	}{
		{"no bearer configured passes", "", nil, http.StatusOK},
		{"missing header refused", bearer, nil, http.StatusUnauthorized},
		{"wrong bearer refused", bearer, []string{"Bearer " + bearer + "x"}, http.StatusUnauthorized},
		{"wrong scheme refused", bearer, []string{"Basic " + bearer}, http.StatusUnauthorized},
		{"two headers refused", bearer, []string{"Bearer " + bearer, "Bearer " + bearer}, http.StatusUnauthorized},
		{"right bearer passes", bearer, []string{"Bearer " + bearer}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.SetAuthToken(tc.bearer)
			h := s.bearerGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			for _, v := range tc.header {
				req.Header.Add("Authorization", v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestBearerGuardCoversTheWholeHandler(t *testing.T) {
	s := NewWithIdentity("100.64.0.10:17712", &fakeController{}, brwidentity.Identity{})
	s.SetAuthToken(strings.Repeat("k", 32))
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/health without a bearer = %d, want 401", rec.Code)
	}
}

func TestBearerGuardCoversWatcherRoutes(t *testing.T) {
	s := NewWithIdentity("100.64.0.10:17712", &fakeController{}, brwidentity.Identity{})
	s.SetAuthToken(strings.Repeat("k", 32))
	for _, path := range []string{"/api/watchers/register", "/api/watchers/manage", "/api/watchers/events"} {
		rec := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, rec.Code)
		}
	}
}
