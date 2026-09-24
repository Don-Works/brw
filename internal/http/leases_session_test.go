package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestLeaseReleaseRenewsLessForAnAbandonedCall(t *testing.T) {
	cases := []struct {
		name       string
		cancelCall bool
		wantTTL    time.Duration
	}{
		{name: "caller stayed for the result", wantTTL: 30 * time.Minute},
		{name: "caller went away mid-call", cancelCall: true, wantTTL: abandonedTabLeaseTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
			leases := newTabLeaseManager(30 * time.Minute)
			leases.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			release, err := leases.acquireFor(ctx, "owner-a", "tab-1", true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancelCall {
				cancel()
			}
			release()

			now = now.Add(tc.wantTTL - time.Second)
			if _, err := leases.acquire("owner-b", "tab-1", false); err == nil {
				t.Fatal("another session took the tab before the lease ran out")
			}
			now = now.Add(2 * time.Second)
			if _, err := leases.acquire("owner-b", "tab-1", false); err != nil {
				t.Fatalf("lease still held %s after the call ended: %v", tc.wantTTL+time.Second, err)
			}
		})
	}
}

// A lease whose holder's call never returned was still enforced after its
// reported expiry: close_tab from another session at 21:48 was refused with
// "leased by another browser session until 21:18:56Z".
func TestExpiredLeaseIsNotEnforced(t *testing.T) {
	cases := []struct {
		name      string
		released  bool
		elapsed   time.Duration
		wantTaken bool
	}{
		{name: "released, before expiry", released: true, elapsed: 29 * time.Minute},
		{name: "released, past expiry", released: true, elapsed: 31 * time.Minute, wantTaken: true},
		{name: "call still running, within the in-flight hold", elapsed: maxInFlightHold - time.Minute},
		{name: "call never returned, past expiry and hold", elapsed: maxInFlightHold + time.Minute, wantTaken: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 9, 24, 20, 48, 56, 0, time.UTC)
			now := start
			ctrl := &recordingCloseController{leaseTestController: leaseTestController{tabs: []browser.Tab{{ID: "235941495"}}}}
			server := New("", ctrl)
			server.leases.now = func() time.Time { return now }

			release, err := server.leases.acquire("owner-a", "235941495", true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.released {
				release()
			}
			now = start.Add(tc.elapsed)

			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodPost, "/api/browser/close", "owner-b", `{"tab_id":"235941495"}`))
			if tc.wantTaken {
				if rec.Code != http.StatusOK || !slices.Equal(ctrl.closed, []string{"235941495"}) {
					t.Fatalf("close by another session = %d %s (closed %v), want it to succeed on an expired lease", rec.Code, rec.Body.String(), ctrl.closed)
				}
				return
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("close by another session = %d %s, want 409 while the lease is held", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			match := regexp.MustCompile(`until (\S+);`).FindStringSubmatch(body.Error)
			if match == nil {
				t.Fatalf("conflict message has no expiry: %q", body.Error)
			}
			until, err := time.Parse(time.RFC3339, match[1])
			if err != nil {
				t.Fatalf("conflict message has no expiry: %q (%v)", body.Error, err)
			}
			if !until.After(now) {
				t.Fatalf("conflict names an expiry %s that is not after now %s", until, now)
			}
		})
	}
}

func TestAbandonedTTLNeverExceedsTheConfiguredTTL(t *testing.T) {
	leases := newTabLeaseManager(time.Minute)
	if leases.abandonedTTL != time.Minute {
		t.Fatalf("abandonedTTL = %s, want the 1m lease TTL", leases.abandonedTTL)
	}
}

// cancelOnCallController cancels the HTTP request's context from inside the
// controller call, the way a gateway killing a script mid-call does.
type cancelOnCallController struct {
	leaseTestController
	cancel context.CancelFunc
	closed []string
}

func (c *cancelOnCallController) Snapshot(ctx context.Context, _ snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	c.cancel()
	<-ctx.Done()
	return snapshot.PageSnapshot{}, ctx.Err()
}

func (c *cancelOnCallController) Open(ctx context.Context, targetURL string) (browser.OpenResult, error) {
	result, err := c.leaseTestController.Open(ctx, targetURL)
	c.cancel()
	return result, err
}

func (c *cancelOnCallController) OpenInGroup(ctx context.Context, targetURL string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	result, err := c.leaseTestController.OpenInGroup(ctx, targetURL, opts)
	c.cancel()
	return result, err
}

func (c *cancelOnCallController) CloseTab(_ context.Context, tabID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, tabID)
	c.tabs = slices.DeleteFunc(c.tabs, func(tab browser.Tab) bool { return tab.ID == tabID })
	return nil
}

func TestCancelledPageCallShortensTheSessionsLease(t *testing.T) {
	ctrl := &cancelOnCallController{leaseTestController: leaseTestController{tabs: []browser.Tab{{ID: "tab-9"}}}}
	server := New("", ctrl)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	server.leases.now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.cancel = cancel
	req := ownerRequest(http.MethodGet, "/api/page/snapshot?tab_id=tab-9", "owner-a", "").WithContext(ctx)
	server.server.Handler.ServeHTTP(httptest.NewRecorder(), req)

	now = now.Add(abandonedTabLeaseTTL + time.Second)
	if !leaseAvailableTo(t, server, "owner-b", "tab-9") {
		t.Fatalf("tab-9 still leased %s after its caller went away", abandonedTabLeaseTTL+time.Second)
	}
}

func TestOpenCancelledMidFlightClosesTheTabItOpened(t *testing.T) {
	ctrl := &cancelOnCallController{}
	server := New("", ctrl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.cancel = cancel

	rec := httptest.NewRecorder()
	req := ownerRequest(http.MethodPost, "/api/browser/open", "owner-a", `{"url":"https://mail.example.com/"}`).WithContext(ctx)
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a cancelled open reported success: %s", rec.Body.String())
	}
	if !slices.Equal(ctrl.closed, []string{"tab-1"}) {
		t.Fatalf("closed tabs = %v, want [tab-1]", ctrl.closed)
	}
	if all, _ := server.leases.ownedTabs("owner-a"); len(all) != 0 {
		t.Fatalf("owner-a still leases %v after its open was cancelled", all)
	}
}

type recordingCloseController struct {
	leaseTestController
	closed []string
}

func (c *recordingCloseController) CloseTab(_ context.Context, tabID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, tabID)
	c.tabs = slices.DeleteFunc(c.tabs, func(tab browser.Tab) bool { return tab.ID == tabID })
	return nil
}

func TestReleaseSessionFreesTheOwnersTabs(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantClosed []string
	}{
		{name: "release only", body: "", wantClosed: []string{}},
		{name: "release and close what the daemon opened", body: `{"close_tabs":true}`, wantClosed: []string{"tab-1", "tab-3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &recordingCloseController{leaseTestController: leaseTestController{tabs: []browser.Tab{{ID: "human"}}}}
			server := New("", ctrl)
			serve := func(req *http.Request) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				server.server.Handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s status = %d, body = %s", req.Method, req.URL.Path, rec.Code, rec.Body.String())
				}
				return rec
			}
			// owner-a: an explicitly opened tab, an automatic working tab, and a
			// claimed tab that existed before the session. owner-b: its own tab.
			serve(ownerRequest(http.MethodPost, "/api/browser/open", "owner-a", `{"url":"https://example.com/"}`))
			serve(ownerRequest(http.MethodPost, "/api/browser/focus", "owner-a", `{"tab_id":"human"}`))
			serve(ownerRequest(http.MethodGet, "/api/page/snapshot", "owner-b", ""))
			server.leases.defaultTabs["owner-a"] = ""
			serve(ownerRequest(http.MethodGet, "/api/page/snapshot", "owner-a", ""))

			rec := serve(ownerRequest(http.MethodPost, "/api/session/release", "owner-a", tc.body))
			var out struct {
				OK       bool     `json:"ok"`
				Released []string `json:"released"`
				Closed   []string `json:"closed"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if !out.OK || !slices.Equal(out.Released, []string{"human", "tab-1", "tab-3"}) {
				t.Fatalf("release = %+v, want ok and released [human tab-1 tab-3]", out)
			}
			wantClosed := tc.wantClosed
			if !slices.Equal(out.Closed, wantClosed) || !slices.Equal(append([]string{}, ctrl.closed...), wantClosed) {
				t.Fatalf("closed = %v (controller saw %v), want %v", out.Closed, ctrl.closed, wantClosed)
			}
			if slices.Contains(ctrl.closed, "human") {
				t.Fatal("a tab the session claimed but did not open was closed")
			}
			if !leaseAvailableTo(t, server, "owner-c", "human") {
				t.Fatal("the claimed tab is still leased after the session was released")
			}
			if all, _ := server.leases.ownedTabs("owner-b"); !slices.Equal(all, []string{"tab-2"}) {
				t.Fatalf("owner-b leases = %v, want [tab-2] untouched", all)
			}
		})
	}
}

func TestReleaseSessionNeedsAnOwner(t *testing.T) {
	server := New("", &leaseTestController{})
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/session/release", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func leaseAvailableTo(t *testing.T, server *Server, owner, tabID string) bool {
	t.Helper()
	release, err := server.leases.acquire(owner, tabID, false)
	if err != nil {
		return false
	}
	release()
	server.leases.release(owner, tabID)
	return true
}
