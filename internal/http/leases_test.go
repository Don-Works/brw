package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestTabLeaseExclusiveAcrossOwners(t *testing.T) {
	leases := newTabLeaseManager(time.Minute)
	releaseA, err := leases.acquire("owner-a", "tab-1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	if _, err := leases.acquire("owner-b", "tab-1", true); err == nil {
		t.Fatal("second owner acquired a live tab lease")
	} else if _, ok := err.(*tabLeaseConflictError); !ok {
		t.Fatalf("conflict error = %T, want *tabLeaseConflictError", err)
	}
	if releaseSameOwner, err := leases.acquire("owner-a", "tab-1", true); err != nil {
		t.Fatalf("same owner could not renew its lease: %v", err)
	} else {
		releaseSameOwner()
	}
}

func TestTabLeaseDoesNotExpireDuringInflightOperation(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	leases := newTabLeaseManager(time.Minute)
	leases.now = func() time.Time { return now }
	releaseA, err := leases.acquire("owner-a", "tab-1", true)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := leases.acquire("owner-b", "tab-1", true); err == nil {
		t.Fatal("an in-flight lease was stolen after its idle deadline")
	}
	releaseA()
	if _, err := leases.acquire("owner-b", "tab-1", true); err == nil {
		t.Fatal("finishing a long operation must renew, not immediately expire, its lease")
	}

	now = now.Add(time.Minute + time.Second)
	releaseB, err := leases.acquire("owner-b", "tab-1", true)
	if err != nil {
		t.Fatalf("idle expired lease was not reclaimable: %v", err)
	}
	releaseB()
}

func TestResolveOrOpenAllocatesOnceForConcurrentFirstCalls(t *testing.T) {
	leases := newTabLeaseManager(time.Minute)
	var opens atomic.Int32
	open := func(context.Context) (browser.OpenResult, error) {
		opens.Add(1)
		time.Sleep(10 * time.Millisecond)
		return browser.OpenResult{Tab: browser.Tab{ID: "tab-new"}}, nil
	}

	start := make(chan struct{})
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tabID, release, err := leases.resolveOrOpen(context.Background(), "owner-a", "", open)
			if err != nil {
				results <- "error:" + err.Error()
				return
			}
			defer release()
			results <- tabID
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for got := range results {
		if got != "tab-new" {
			t.Fatalf("resolved tab = %q, want tab-new", got)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("automatic opens = %d, want 1", got)
	}
}

type leaseTestController struct {
	fakeController
	mu           sync.Mutex
	tabs         []browser.Tab
	nextTab      int
	snapshotTabs []string
	focusTabs    []string
	groupOpts    []browser.TabGroupOptions
}

func (c *leaseTestController) Open(_ context.Context, targetURL string) (browser.OpenResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextTab++
	tab := browser.Tab{ID: "tab-" + strconv.Itoa(c.nextTab), URL: targetURL}
	c.tabs = append(c.tabs, tab)
	return browser.OpenResult{Tab: tab, Ready: true}, nil
}

// OpenInGroup mirrors the extension bridge: a fresh tab that lands in the
// requested (title-keyed) group. Without this override the embedded
// fakeController would hand every session the SAME fixed tab id, which the
// lease layer correctly rejects as contended.
func (c *leaseTestController) OpenInGroup(ctx context.Context, targetURL string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	result, err := c.Open(ctx, targetURL)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupOpts = append(c.groupOpts, opts)
	result.Tab.GroupID = "group-" + opts.Name
	result.Tab.GroupTitle = opts.Name
	result.Tab.GroupColor = opts.Color
	for i := range c.tabs {
		if c.tabs[i].ID == result.Tab.ID {
			c.tabs[i] = result.Tab
		}
	}
	return result, nil
}

func (c *leaseTestController) ListTabs(context.Context) ([]browser.Tab, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]browser.Tab(nil), c.tabs...), nil
}

func (c *leaseTestController) Snapshot(ctx context.Context, _ snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshotTabs = append(c.snapshotTabs, browser.TabIDFromContext(ctx))
	return sampleSnapshot(), nil
}

func (c *leaseTestController) FocusTab(_ context.Context, tabID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.focusTabs = append(c.focusTabs, tabID)
	return nil
}

func ownerRequest(method, path, owner, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set(usagelog.HeaderOwnerID, owner)
	if body != "" {
		req.Header.Set("content-type", "application/json")
	}
	return req
}

func TestHTTPLeaseKeepsConcurrentSessionsOnDifferentTabs(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)

	for _, owner := range []string{"owner-a", "owner-b"} {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/page/snapshot", owner, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s initial snapshot status = %d, body = %s", owner, rec.Code, rec.Body.String())
		}
	}
	ctrl.mu.Lock()
	gotTabs := append([]string(nil), ctrl.snapshotTabs...)
	ctrl.mu.Unlock()
	if len(gotTabs) != 2 || gotTabs[0] != "tab-1" || gotTabs[1] != "tab-2" {
		t.Fatalf("session snapshot tabs = %v, want [tab-1 tab-2]", gotTabs)
	}

	// Even a read-only operation may not cross into another session's tab.
	conflict := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(conflict, ownerRequest(http.MethodGet, "/api/page/snapshot?tab_id=tab-2", "owner-a", ""))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("cross-session read status = %d, want 409; body = %s", conflict.Code, conflict.Body.String())
	}
	if conflict.Header().Get(usagelog.HeaderErrorClass) != "tab_contended" {
		t.Fatalf("error class = %q, want tab_contended", conflict.Header().Get(usagelog.HeaderErrorClass))
	}
	ctrl.mu.Lock()
	if ctrl.nextTab != 2 {
		ctrl.mu.Unlock()
		t.Fatalf("explicit contention silently allocated another tab; opens = %d, want 2", ctrl.nextTab)
	}
	ctrl.mu.Unlock()

	focus := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(focus, ownerRequest(http.MethodPost, "/api/browser/focus", "owner-a", `{"tab_id":"tab-2"}`))
	if focus.Code != http.StatusConflict {
		t.Fatalf("cross-session focus status = %d, want 409; body = %s", focus.Code, focus.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(ctrl.focusTabs) != 0 {
		t.Fatalf("contended focus reached controller: %v", ctrl.focusTabs)
	}
}

type lostTabController struct {
	leaseTestController
}

func (c *lostTabController) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return snapshot.PageSnapshot{}, errors.New("No tab with id 41")
}

func TestLostLeasedTabIsReleasedForSafeNextCallRecovery(t *testing.T) {
	ctrl := &lostTabController{}
	server := New("", ctrl)
	for attempt := 1; attempt <= 2; attempt++ {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/page/snapshot", "owner-a", ""))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d status = %d, want 400; body = %s", attempt, rec.Code, rec.Body.String())
		}
		if rec.Header().Get(usagelog.HeaderErrorClass) != "tab_lost" {
			t.Fatalf("attempt %d error class = %q, want tab_lost", attempt, rec.Header().Get(usagelog.HeaderErrorClass))
		}
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.nextTab != 2 {
		t.Fatalf("next call reused stale closed tab: automatic opens = %d, want 2", ctrl.nextTab)
	}
}

func TestPlanCannotFocusAnotherSessionsTab(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)
	for _, owner := range []string{"owner-a", "owner-b"} {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/page/snapshot", owner, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("allocate %s: %s", owner, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodPost, "/api/page/execute_plan", "owner-a", `{"steps":[{"action":"focus_tab","id":"tab-2"}]}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("plan focus contention status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if len(ctrl.planSteps) != 0 {
		t.Fatalf("contended plan reached controller: %+v", ctrl.planSteps)
	}
}

func TestListTabsReportsOwnerRedactedLeaseStatus(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)
	for _, owner := range []string{"owner-a", "owner-b"} {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/page/snapshot", owner, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("allocate %s: %s", owner, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/browser/tabs", "owner-a", ""))
	var tabs []browser.Tab
	if err := json.NewDecoder(rec.Body).Decode(&tabs); err != nil {
		t.Fatal(err)
	}
	statuses := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		if tab.Lease == nil {
			t.Fatalf("tab %s has no lease metadata", tab.ID)
		}
		statuses = append(statuses, tab.ID+":"+tab.Lease.Status)
	}
	sort.Strings(statuses)
	if got, want := statuses, []string{"tab-1:mine", "tab-2:leased"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("lease statuses = %v, want %v", got, want)
	}
}

func TestSimultaneousExplicitClaimsHaveSingleWinner(t *testing.T) {
	ctrl := &leaseTestController{tabs: []browser.Tab{{ID: "shared"}}}
	server := New("", ctrl)
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"owner-a", "owner-b"} {
		owner := owner
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, ownerRequest(http.MethodGet, "/api/page/snapshot?tab_id=shared", owner, ""))
			statuses <- rec.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	var got []int
	for status := range statuses {
		got = append(got, status)
	}
	sort.Ints(got)
	if len(got) != 2 || got[0] != http.StatusOK || got[1] != http.StatusConflict {
		t.Fatalf("simultaneous statuses = %v, want [200 409]", got)
	}
}
