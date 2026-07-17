package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestSanitizeAgentName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "claude-code", "claude-code"},
		{"spaces and case kept", "Claude Code", "Claude-Code"},
		{"strips header noise", "evil\r\nX-Other: 1", "evil-X-Other-1"},
		{"strips secret-looking punctuation", "tok=abc!!&", "tok-abc"},
		{"collapses runs", "a   ///  b", "a-b"},
		{"trims separators", "--name--", "name"},
		{"caps length", strings.Repeat("a", 40), strings.Repeat("a", 24)},
		{"empty", "   ", ""},
		{"only junk", "!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeAgentName(tt.in); got != tt.want {
				t.Fatalf("sanitizeAgentName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestOwnerGroupOptionsStableAndDistinct(t *testing.T) {
	a1 := ownerGroupOptions("owner-a", "claude-code")
	a2 := ownerGroupOptions("owner-a", "claude-code")
	if a1 != a2 {
		t.Fatalf("options for the same owner are not stable: %+v vs %+v", a1, a2)
	}
	if !strings.HasPrefix(a1.Name, "claude-code-") {
		t.Fatalf("title = %q, want claude-code-<suffix>", a1.Name)
	}

	// Two agents reporting the SAME display name must land in different groups:
	// the extension reuses same-title groups within a window.
	b := ownerGroupOptions("owner-b", "claude-code")
	if b.Name == a1.Name {
		t.Fatalf("two owners with one display name share the group title %q", b.Name)
	}

	valid := make(map[string]bool, len(tabGroupColors))
	for _, color := range tabGroupColors {
		valid[color] = true
	}
	if !valid[a1.Color] || !valid[b.Color] {
		t.Fatalf("colors %q/%q not in Chrome's tabGroups enum", a1.Color, b.Color)
	}
}

func TestOwnerGroupOptionsFallbackWithoutAgentName(t *testing.T) {
	got := ownerGroupOptions("owner-a", "")
	if !strings.HasPrefix(got.Name, "brw-") || len(got.Name) <= len("brw-") {
		t.Fatalf("fallback title = %q, want brw-<suffix>", got.Name)
	}
	if junk := ownerGroupOptions("owner-a", "!!!"); junk.Name != got.Name {
		t.Fatalf("unusable display name should fall back identically: %q vs %q", junk.Name, got.Name)
	}
}

func agentRequest(method, path, owner, agentName, body string) *http.Request {
	req := ownerRequest(method, path, owner, body)
	if agentName != "" {
		req.Header.Set(usagelog.HeaderAgentName, agentName)
	}
	return req
}

func TestAutoAllocatedTabsJoinPerAgentGroups(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)

	// Two sessions that report the SAME display name each trigger one automatic
	// working-tab allocation.
	for _, owner := range []string{"owner-a", "owner-b"} {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, agentRequest(http.MethodGet, "/api/page/snapshot", owner, "claude-code", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s snapshot status = %d, body = %s", owner, rec.Code, rec.Body.String())
		}
	}

	ctrl.mu.Lock()
	opts := append([]browser.TabGroupOptions(nil), ctrl.groupOpts...)
	ctrl.mu.Unlock()
	if len(opts) != 2 {
		t.Fatalf("grouped opens = %d, want 2 (one per session)", len(opts))
	}
	for _, o := range opts {
		if !strings.HasPrefix(o.Name, "claude-code-") {
			t.Fatalf("group title = %q, want claude-code-<suffix>", o.Name)
		}
	}
	if opts[0].Name == opts[1].Name {
		t.Fatalf("both sessions were funneled into one group %q; want separate per-agent groups", opts[0].Name)
	}
}

func TestExplicitOpenDefaultsToOwnerGroup(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)

	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, agentRequest(http.MethodPost, "/api/browser/open", "owner-a", "claude-code", `{"url":"https://example.com"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(ctrl.groupOpts) != 1 || !strings.HasPrefix(ctrl.groupOpts[0].Name, "claude-code-") {
		t.Fatalf("group opts = %+v, want one per-agent group open", ctrl.groupOpts)
	}
}

func TestExplicitGroupWinsOverOwnerGroup(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)

	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, agentRequest(http.MethodPost, "/api/browser/open", "owner-a", "claude-code", `{"url":"https://example.com","group":"my-run"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(ctrl.groupOpts) != 1 || ctrl.groupOpts[0].Name != "my-run" {
		t.Fatalf("group opts = %+v, want the caller's explicit group", ctrl.groupOpts)
	}
}

// groupUnsupportedController mimics the direct-CDP transport: grouping is
// impossible, opening is not.
type groupUnsupportedController struct {
	leaseTestController
}

func (c *groupUnsupportedController) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{}, browser.ErrTabGroupingUnsupported
}

func TestAutoAllocationFallsBackWhenGroupingUnsupported(t *testing.T) {
	ctrl := &groupUnsupportedController{}
	server := New("", ctrl)

	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, agentRequest(http.MethodGet, "/api/page/snapshot", "owner-a", "claude-code", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body = %s; grouping-incapable transport must still allocate", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.nextTab != 1 {
		t.Fatalf("plain opens = %d, want 1 fallback open", ctrl.nextTab)
	}
}

func TestListTabsFlagsGroupDrift(t *testing.T) {
	ctrl := &leaseTestController{}
	server := New("", ctrl)

	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, agentRequest(http.MethodGet, "/api/page/snapshot", "owner-a", "claude-code", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("allocate: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A human drags the tab out of the agent's group.
	ctrl.mu.Lock()
	expected := ctrl.tabs[0].GroupID
	ctrl.tabs[0].GroupID = "group-humans-own"
	ctrl.mu.Unlock()

	list := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(list, agentRequest(http.MethodGet, "/api/browser/tabs", "owner-a", "claude-code", ""))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", list.Code, list.Body.String())
	}
	var tabs []browser.Tab
	if err := json.Unmarshal(list.Body.Bytes(), &tabs); err != nil {
		t.Fatalf("decode tabs: %v; body = %s", err, list.Body.String())
	}
	if len(tabs) != 1 || tabs[0].Lease == nil {
		t.Fatalf("tabs = %+v, want one annotated tab", tabs)
	}
	if !tabs[0].Lease.GroupDrift || tabs[0].Lease.ExpectedGroupID != expected {
		t.Fatalf("lease = %+v, want group_drift with expected_group_id %q", tabs[0].Lease, expected)
	}

	// Another session sees plain "leased" with no drift chatter.
	other := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(other, agentRequest(http.MethodGet, "/api/browser/tabs", "owner-b", "", ""))
	var otherTabs []browser.Tab
	if err := json.Unmarshal(other.Body.Bytes(), &otherTabs); err != nil {
		t.Fatalf("decode other tabs: %v", err)
	}
	if len(otherTabs) != 1 || otherTabs[0].Lease == nil || otherTabs[0].Lease.GroupDrift || otherTabs[0].Lease.Status != "leased" {
		t.Fatalf("other session lease = %+v, want leased without drift", otherTabs[0].Lease)
	}
}
