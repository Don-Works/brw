package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/pagewatch"
)

type pageWatchAPI struct {
	registered pagewatch.RegisterOptions
	managed    pagewatch.ManageOptions
	events     pagewatch.EventsOptions
}

func (a *pageWatchAPI) WatchPage(_ context.Context, req pagewatch.RegisterOptions) (pagewatch.Watcher, error) {
	a.registered = req
	return pagewatch.Watcher{RegisterOptions: req, Enabled: true, Status: "watching", Seq: 9}, nil
}
func (a *pageWatchAPI) PageWatchers(_ context.Context, req pagewatch.ManageOptions) (pagewatch.ManageResult, error) {
	a.managed = req
	return pagewatch.ManageResult{Watchers: []pagewatch.Watcher{}, Removed: req.Action == "remove"}, nil
}
func (a *pageWatchAPI) PageEvents(_ context.Context, req pagewatch.EventsOptions) (pagewatch.EventsResult, error) {
	a.events = req
	return pagewatch.EventsResult{WatcherID: req.WatcherID, Events: []pagewatch.Event{{WatcherID: req.WatcherID, Seq: 9, Kind: "unavailable", Reason: "url_mismatch"}}, LatestSeq: 9, OldestSeq: 9, Gap: true}, nil
}

func TestPageWatcherHTTPProxySurvivesClientReplacementWithoutTabLease(t *testing.T) {
	api := &pageWatchAPI{}
	server := New("127.0.0.1:0", &fakeController{})
	server.SetPageWatchAPI(api)
	host := httptest.NewServer(server.server.Handler)
	defer host.Close()
	first, err := httpclient.New(host.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := pagewatch.RegisterOptions{ID: "identity-owned", URL: "https://chat.test/inbox", Mode: "title", IntervalMS: 5000, RefreshIntervalMS: 30000}
	w, err := first.WatchPage(context.Background(), opts)
	if err != nil || w.ID != opts.ID || w.Seq != 9 || api.registered != opts {
		t.Fatalf("register: %+v %v", w, err)
	}
	second, err := httpclient.New(host.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out, err := second.PageEvents(context.Background(), pagewatch.EventsOptions{WatcherID: w.ID, SinceSeq: 8, Limit: 20})
	if err != nil || len(out.Events) != 1 || out.Events[0].Reason != "url_mismatch" || !out.Gap || api.events.SinceSeq != 8 {
		t.Fatalf("events: %+v %v", out, err)
	}
	managed, err := second.PageWatchers(context.Background(), pagewatch.ManageOptions{Action: "remove", ID: w.ID})
	if err != nil || !managed.Removed || api.managed.ID != w.ID {
		t.Fatalf("remove: %+v %v", managed, err)
	}
	if server.InFlight() != 0 || len(server.leases.byTab) != 0 {
		t.Fatal("watcher API opened an agent tab or acquired a disposable lease")
	}
	bad := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/watchers/register", strings.NewReader(`{"url":"https://chat.test/","expression":"arbitrary JS"}`))
	server.server.Handler.ServeHTTP(bad, request)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown argument accepted: %d %s", bad.Code, bad.Body.String())
	}
}

func TestPageWatcherHTTPReportsDisabledCapability(t *testing.T) {
	server := New("", &fakeController{})
	rec := doJSON(t, server, http.MethodPost, "/api/watchers/manage", `{"action":"list"}`)
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("disabled watcher API reported success: %d %s", rec.Code, rec.Body.String())
	}
}
