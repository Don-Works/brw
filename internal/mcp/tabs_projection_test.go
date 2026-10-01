package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

type tabProjectionController struct {
	fakeController
	tabs  []browser.Tab
	err   error
	calls int
}

func (c *tabProjectionController) ListTabs(context.Context) ([]browser.Tab, error) {
	c.calls++
	return c.tabs, c.err
}

func tabProjectionFixture(count int) []browser.Tab {
	tabs := make([]browser.Tab, count)
	for i := range tabs {
		status := []string{"mine", "leased", "available", "unknown"}[i%4]
		tabs[i] = browser.Tab{
			ID: fmt.Sprintf("tab-%04d", i), Title: fmt.Sprintf("Synthetic Invoice %04d — 測試", i), URL: fmt.Sprintf("https://example.test/invoices/%04d?view=summary", i), Type: "page", WindowID: i/20 + 1,
			WindowType: "normal", GroupID: fmt.Sprintf("group-%d", i/10), GroupTitle: "Synthetic work", GroupColor: "blue", GroupCollapsed: i%3 == 0,
			Active: i == 0, Highlighted: i%20 == 0, WindowFocused: i < 20, OpenerTabID: "synthetic-opener", Popup: i%7 == 0, Discarded: i%11 == 0, Frozen: i%13 == 0,
			BrowserContextID: fmt.Sprintf("context-%d", i/100),
		}
		if status != "unknown" {
			tabs[i].Lease = &browser.TabLeaseInfo{Status: status, Mine: status == "mine", ExpiresAt: "2026-10-01T18:00:00Z", GroupDrift: status == "mine", ExpectedGroupID: "expected-group"}
		}
	}
	return tabs
}

func decodeCompactTabResult(t *testing.T, result any) compactTabList {
	t.Helper()
	object, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type %T", result)
	}
	if _, duplicated := object["structuredContent"]; duplicated {
		t.Fatal("compact projection duplicated structured content")
	}
	content, ok := object["content"].([]toolContent)
	if !ok || len(content) != 1 || content[0].Type != "text" {
		t.Fatalf("content type %T", object["content"])
	}
	var projection compactTabList
	if err := json.Unmarshal([]byte(content[0].Text), &projection); err != nil {
		t.Fatal(err)
	}
	return projection
}

func TestTabProjectionPreservesLegacyDefault(t *testing.T) {
	for _, args := range []json.RawMessage{nil, []byte(`{}`), []byte(`null`), []byte(`{"format":"json"}`)} {
		controller := &tabProjectionController{tabs: tabProjectionFixture(3)}
		server := New(controller)
		got, rpcErr := server.callTool(context.Background(), "brw_list_tabs", args)
		if rpcErr != nil || controller.calls != 1 {
			t.Fatalf("err=%v calls=%d", rpcErr, controller.calls)
		}
		want, wantErr := toolJSON(controller.tabs, nil)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if wantErr != nil || !bytes.Equal(gotJSON, wantJSON) {
			t.Fatal("default response changed")
		}
	}
}

func TestTabProjectionRejectsOptionsBeforeListing(t *testing.T) {
	for _, args := range []string{`{"format":"other"}`, `{"query":"invoice"}`, `{"limit":40}`, `{"owned":false}`, `{"format":"compact","limit":0}`, `{"format":"compact","limit":-1}`, `{"format":"compact","limit":1001}`, `{"format":"compact","limit":1.5}`, `{"format":"compact","owned":"true"}`, `{"format":"compact","query":true}`, `{"format":"compact","query":"` + strings.Repeat("雪", 257) + `"}`} {
		controller := &tabProjectionController{}
		_, rpcErr := New(controller).callTool(context.Background(), "brw_list_tabs", []byte(args))
		if rpcErr == nil || controller.calls != 0 {
			t.Fatalf("args=%s err=%v calls=%d", args, rpcErr, controller.calls)
		}
	}
	query := strings.Repeat("雪", 256)
	limit := 1000
	if _, err := (tabListRequest{Format: "compact", Query: &query, Limit: &limit}).options(); err != nil {
		t.Fatalf("valid maximum boundary rejected: %v", err)
	}
}

func TestTabProjectionSchemaAdvertisesBounds(t *testing.T) {
	props := toolProperties(t, "brw_list_tabs")
	for _, key := range []string{"format", "query", "limit", "owned"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema omits %s", key)
		}
	}
	limit := props["limit"].(map[string]any)
	query := props["query"].(map[string]any)
	if limit["type"] != "integer" || limit["minimum"] != 1 || limit["maximum"] != 1000 || query["maxLength"] != 256 {
		t.Fatal("schema bounds disagree with validation")
	}
}

func TestTabProjectionIdentityAndOwnership(t *testing.T) {
	tabs := tabProjectionFixture(4)
	tabs[0].ID = "id\nwith\"quotes"
	tabs[0].Title = "完整 title\n" + strings.Repeat("雪", 1000)
	tabs[0].URL = "https://example.test/" + strings.Repeat("path", 1000) + "?q=\"value\""
	before, _ := json.Marshal(tabs)
	controller := &tabProjectionController{tabs: tabs}
	result, rpcErr := New(controller).callTool(context.Background(), "brw_list_tabs", []byte(`{"format":"compact","limit":1000}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	projection := decodeCompactTabResult(t, result)
	if projection.Total != 4 || projection.Matched != 4 || projection.Returned != 4 || projection.Truncated || projection.OwnershipUnknown != 1 {
		t.Fatalf("coverage=%+v", projection)
	}
	for i, tab := range projection.Tabs {
		original := tabs[i]
		if tab.ID != original.ID || tab.Title != original.Title || tab.URL != original.URL || tab.BrowserContextID != original.BrowserContextID || tab.Lease != tabLeaseStatus(original) || tab.Active != original.Active || tab.Discarded != original.Discarded || tab.Frozen != original.Frozen {
			t.Fatalf("identity/state lost at %d", i)
		}
	}
	if !projection.Tabs[0].GroupDrift || projection.Tabs[0].ExpectedGroupID != tabs[0].Lease.ExpectedGroupID {
		t.Fatal("group drift identity lost")
	}
	after, _ := json.Marshal(tabs)
	if !bytes.Equal(before, after) {
		t.Fatal("projection mutated original tabs")
	}
}

func TestTabProjectionFiltersAndBounds(t *testing.T) {
	tabs := tabProjectionFixture(1000)
	for _, test := range []struct {
		args      string
		matched   int
		returned  int
		truncated bool
		firstID   string
	}{
		{`{"format":"compact"}`, 1000, 40, true, "tab-0000"},
		{`{"format":"compact","owned":true,"limit":2}`, 250, 2, true, "tab-0000"},
		{`{"format":"compact","owned":false,"limit":1000}`, 1000, 1000, false, "tab-0000"},
		{`{"format":"compact","query":"TAB-0001"}`, 1, 1, false, "tab-0001"},
		{`{"format":"compact","query":" INVOICE 0002 "}`, 1, 1, false, "tab-0002"},
		{`{"format":"compact","query":"/invoices/0003?VIEW="}`, 1, 1, false, "tab-0003"},
		{`{"format":"compact","query":"tab-0001","owned":true}`, 0, 0, false, ""},
		{`{"format":"compact","query":"no matches"}`, 0, 0, false, ""},
	} {
		controller := &tabProjectionController{tabs: tabs}
		result, rpcErr := New(controller).callTool(context.Background(), "brw_list_tabs", []byte(test.args))
		if rpcErr != nil || controller.calls != 1 {
			t.Fatalf("args=%s err=%v calls=%d", test.args, rpcErr, controller.calls)
		}
		projection := decodeCompactTabResult(t, result)
		if projection.Total != 1000 || projection.OwnershipUnknown != 250 || projection.Matched != test.matched || projection.Returned != test.returned || projection.Truncated != test.truncated {
			t.Fatalf("args=%s total=%d matched=%d returned=%d truncated=%v unknown=%d", test.args, projection.Total, projection.Matched, projection.Returned, projection.Truncated, projection.OwnershipUnknown)
		}
		if len(projection.Tabs) > 0 && projection.Tabs[0].ID != test.firstID {
			t.Fatalf("order changed: first=%s", projection.Tabs[0].ID)
		}
	}
	projection := projectTabList(nil, tabListOptions{compact: true, limit: 40})
	if projection.Tabs == nil || projection.Total != 0 || projection.Truncated {
		t.Fatal("empty projection is not an explicit empty list")
	}
}

func TestTabProjectionPreservesControllerError(t *testing.T) {
	controller := &tabProjectionController{tabs: tabProjectionFixture(4), err: errors.New("synthetic ownership listing failure")}
	result, rpcErr := New(controller).callTool(context.Background(), "brw_list_tabs", []byte(`{"format":"compact","owned":true}`))
	want, wantErr := toolJSON(controller.tabs, controller.err)
	if rpcErr != wantErr || !reflect.DeepEqual(result, want) || controller.calls != 1 {
		t.Fatal("controller error was hidden by projection")
	}
	unknown := tabProjectionFixture(1)
	unknown[0].Lease = &browser.TabLeaseInfo{Status: "unexpected", Mine: true}
	projection := projectTabList(unknown, tabListOptions{compact: true, limit: 40, owned: true})
	if projection.OwnershipUnknown != 1 || projection.Matched != 0 || projection.Returned != 0 {
		t.Fatal("unknown status was inferred owned from a separate flag")
	}
}

func BenchmarkTabProjection1000(b *testing.B) {
	for _, test := range []struct {
		name string
		args string
	}{
		{"legacy", `{}`},
		{"compact_1000", `{"format":"compact","limit":1000}`},
		{"compact_default40", `{"format":"compact"}`},
		{"compact_owned40", `{"format":"compact","owned":true}`},
		{"compact_query1", `{"format":"compact","query":"invoice 0500"}`},
	} {
		b.Run(test.name, func(b *testing.B) {
			server := New(&tabProjectionController{tabs: tabProjectionFixture(1000)})
			args := json.RawMessage(test.args)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, rpcErr := server.callTool(context.Background(), "brw_list_tabs", args)
				if rpcErr != nil {
					b.Fatal(rpcErr)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(encoded)), "output_bytes")
			}
		})
	}
}
