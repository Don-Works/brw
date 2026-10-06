package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/pagewatch"
)

type pageWatchController struct {
	fakeController
	calls []string
}

func (c *pageWatchController) WatchPage(_ context.Context, req pagewatch.RegisterOptions) (pagewatch.Watcher, error) {
	c.calls = append(c.calls, "register")
	return pagewatch.Watcher{RegisterOptions: req, Seq: 5}, nil
}
func (c *pageWatchController) PageWatchers(_ context.Context, req pagewatch.ManageOptions) (pagewatch.ManageResult, error) {
	c.calls = append(c.calls, req.Action)
	return pagewatch.ManageResult{Watchers: []pagewatch.Watcher{}}, nil
}
func (c *pageWatchController) PageEvents(_ context.Context, req pagewatch.EventsOptions) (pagewatch.EventsResult, error) {
	c.calls = append(c.calls, "events")
	return pagewatch.EventsResult{WatcherID: req.WatcherID, Events: []pagewatch.Event{{WatcherID: req.WatcherID, Kind: "recovered", Seq: 5}}, LatestSeq: 5}, nil
}

func TestPersistentWatcherMCPUsesOptionalControllerCapability(t *testing.T) {
	c := &pageWatchController{}
	server := New(c)
	for _, call := range []struct{ name, args string }{{"brw_watch_page", `{"id":"stable","url":"https://chat.test/inbox","refresh_interval_ms":30000}`}, {"brw_page_watchers", `{"action":"pause","id":"stable"}`}, {"brw_page_events", `{"watcher_id":"stable","since_seq":4}`}} {
		out, rpcErr := server.callTool(context.Background(), call.name, json.RawMessage(call.args))
		if rpcErr != nil {
			t.Fatalf("%s: %+v", call.name, rpcErr)
		}
		encoded, _ := json.Marshal(out)
		if strings.Contains(string(encoded), `"isError":true`) {
			t.Fatalf("%s failed: %s", call.name, encoded)
		}
	}
	if strings.Join(c.calls, ",") != "register,pause,events" {
		t.Fatalf("wrong capability dispatch: %v", c.calls)
	}
	for _, name := range []string{"brw_watch_page", "brw_page_watchers", "brw_page_events"} {
		if !tabAgnosticTools[name] {
			t.Errorf("%s incorrectly resolves caller tab", name)
		}
		if !catalogueHasTool(tools(), name) {
			t.Errorf("%s missing from tools/list", name)
		}
	}
	if out, _ := New(&fakeController{}).callTool(context.Background(), "brw_page_watchers", json.RawMessage(`{}`)); !strings.Contains(string(mustWatcherJSON(t, out)), "disabled") {
		t.Fatal("disabled capability was not reported")
	}
}

func mustWatcherJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
