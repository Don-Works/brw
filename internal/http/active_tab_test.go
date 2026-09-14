package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

// activeTabController names a tab that is deliberately NOT the one the lease
// manager hands a session, so the two sources are distinguishable.
type activeTabController struct {
	*fakeController
	tabID string
	err   error
}

func (c *activeTabController) ActiveTabID(context.Context) (string, error) {
	return c.tabID, c.err
}

func activeTabRequest(owner string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/browser/active_tab", nil)
	if owner != "" {
		req.Header.Set(usagelog.HeaderOwnerID, owner)
	}
	return req
}

// The route exists so a proxying daemon can name the tab its page-tool report
// has to be polled back into. What it must name is the tab a page call from THAT
// caller lands in — which, for a session holding a working tab, is its lease and
// not the browser's own active tab. Naming the wrong tab is worse than naming
// none: the agent polls a document that never held the invocation and reads back
// status "lost".
func TestActiveTabRouteNamesTheTabAPageCallWouldLandIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controller browser.Controller
		owner      string
		takeLease  bool
		wantStatus int
		wantTabID  string
		wantError  string
	}{
		{
			name:       "a session's working tab wins over the browser's active tab",
			controller: &activeTabController{fakeController: &fakeController{}, tabID: "browser-active"},
			owner:      "owner-a",
			takeLease:  true,
			wantStatus: http.StatusOK,
			// fakeController.OpenInGroup opens tab1 for the session's lease.
			wantTabID: "tab1",
		},
		{
			name:       "with no session the controller names the tab",
			controller: &activeTabController{fakeController: &fakeController{}, tabID: "browser-active"},
			wantStatus: http.StatusOK,
			wantTabID:  "browser-active",
		},
		{
			name:       "a session that holds no tab falls back to the controller",
			controller: &activeTabController{fakeController: &fakeController{}, tabID: "browser-active"},
			owner:      "owner-b",
			wantStatus: http.StatusOK,
			wantTabID:  "browser-active",
		},
		{
			name:       "a controller with nothing to name says so",
			controller: &activeTabController{fakeController: &fakeController{}, err: errors.New("no tab is open to name")},
			wantStatus: http.StatusBadRequest,
			wantError:  "no tab is open to name",
		},
		{
			name:       "a transport without the capability is refused by name",
			controller: &fakeController{},
			wantStatus: http.StatusBadRequest,
			wantError:  "cannot name its active tab",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := New("", tc.controller)
			if tc.takeLease {
				// One lease-scoped call first, which is what opens the session's
				// working tab. A page-tool report only ever asks after one.
				rec := httptest.NewRecorder()
				server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodPost, "/api/page/evaluate", tc.owner, `{"expression":"1"}`))
				if rec.Code != http.StatusOK {
					t.Fatalf("lease-establishing evaluate status = %d, body = %s", rec.Code, rec.Body.String())
				}
			}

			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, activeTabRequest(tc.owner))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantError != "" {
				if !strings.Contains(rec.Body.String(), tc.wantError) {
					t.Fatalf("refusal %q does not name the reason %q", rec.Body.String(), tc.wantError)
				}
				return
			}
			var got struct {
				TabID string `json:"tab_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if got.TabID != tc.wantTabID {
				t.Fatalf("tab_id = %q, want %q", got.TabID, tc.wantTabID)
			}
		})
	}
}
