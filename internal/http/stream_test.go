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

// The daemon is shared. An unscoped stream would hand one agent session
// another's browsing live — on a signed-in profile that means authenticated
// page titles and URLs. Scoping must match /api/page/trace exactly.
func TestSessionStreamScopesEntriesToTheCallersLeases(t *testing.T) {
	s := New("127.0.0.1:0", nonStreamingController{})

	tests := []struct {
		name  string
		owner string
		tabID string
		owned bool
		want  bool
	}{
		{"tab-less entry is daemon-wide", "session-a", "", false, true},
		{"tab-less entry visible without any lease identity", "", "", false, true},
		{"own tab is visible", "session-a", "tab-1", true, true},
		{"another session's tab is withheld", "session-a", "tab-2", false, false},
		{"no lease identity withholds every tab entry", "", "tab-1", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.owned {
				if _, err := s.leases.acquire(tt.owner, tt.tabID, false); err != nil {
					t.Fatal(err)
				}
			}
			got := tt.tabID == "" || (tt.owner != "" && s.leases.ownsTab(tt.owner, tt.tabID))
			if got != tt.want {
				t.Fatalf("visible=%v, want %v", got, tt.want)
			}
		})
	}
}
