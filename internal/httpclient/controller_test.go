package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestNew_EmptyURL(t *testing.T) {
	_, err := New("", 0)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://bad", 0)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNew_AddsHTTPScheme(t *testing.T) {
	c, err := New("localhost:1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://localhost:1234" {
		t.Fatalf("expected http:// prefix, got %q", c.baseURL)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c, err := New("http://localhost:1234/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://localhost:1234" {
		t.Fatalf("expected trimmed URL, got %q", c.baseURL)
	}
}

func TestNew_DefaultTimeout(t *testing.T) {
	c, err := New("http://localhost:1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.client.Timeout != 20*time.Second {
		t.Fatalf("expected 20s timeout, got %v", c.client.Timeout)
	}
}

func TestUpstreamErrorsAndJSONAreBoundedAndStrict(t *testing.T) {
	t.Run("bounded error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": strings.Repeat("x", 1<<20)})
		}))
		defer srv.Close()
		controller, err := New(srv.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.ListTabs(context.Background())
		if err == nil || len(err.Error()) > maxUpstreamErrorBytes+32 || !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("error length=%d error=%v", len(err.Error()), err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[] {}`))
		}))
		defer srv.Close()
		controller, err := New(srv.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := controller.ListTabs(context.Background()); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("trailing JSON error = %v", err)
		}
	})
}

func TestRequestsCarryOnlyNonSecretCorrelationMetadata(t *testing.T) {
	var session, owner, firstRequest, secondRequest, client string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session == "" {
			session = r.Header.Get(usagelog.HeaderSessionID)
			owner = r.Header.Get(usagelog.HeaderOwnerID)
			firstRequest = r.Header.Get(usagelog.HeaderRequestID)
			client = r.Header.Get(usagelog.HeaderClient)
		} else {
			if got := r.Header.Get(usagelog.HeaderSessionID); got != session {
				t.Errorf("session changed: %q != %q", got, session)
			}
			secondRequest = r.Header.Get(usagelog.HeaderRequestID)
		}
		json.NewEncoder(w).Encode([]browser.Tab{})
	}))
	defer srv.Close()
	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTabs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTabs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if usagelog.SafeID(session) == "" || usagelog.SafeID(owner) == "" || firstRequest == "" || secondRequest == "" || firstRequest == secondRequest {
		t.Fatalf("bad correlation metadata: session=%q owner=%q first=%q second=%q", session, owner, firstRequest, secondRequest)
	}
	if client != "brw-httpclient" {
		t.Fatalf("client = %q", client)
	}
}

func TestAgentNameHeaderForwarding(t *testing.T) {
	var gotName string
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get(usagelog.HeaderAgentName)
		_, sawHeader = r.Header[usagelog.HeaderAgentName]
		json.NewEncoder(w).Encode([]browser.Tab{})
	}))
	defer srv.Close()

	t.Setenv("BRW_AGENT_NAME", "")
	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTabs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawHeader {
		t.Fatalf("agent-name header sent with no name configured: %q", gotName)
	}

	c.SetAgentName("Claude Code\r\nX-Evil: 1")
	if _, err := c.ListTabs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotName != "Claude-CodeX-Evil-1" {
		t.Fatalf("sanitized agent name = %q", gotName)
	}

	// The first installed name wins; a later MCP initialize cannot rename the
	// session's group mid-run.
	c.SetAgentName("other")
	if _, err := c.ListTabs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotName != "Claude-CodeX-Evil-1" {
		t.Fatalf("agent name changed after first install: %q", gotName)
	}
}

func TestAgentNameEnvOverridesClientInfo(t *testing.T) {
	t.Setenv("BRW_AGENT_NAME", "ops-agent")
	c, err := New("http://localhost:1234", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetAgentName("claude-code")
	if got, _ := c.agentName.Load().(string); got != "ops-agent" {
		t.Fatalf("agent name = %q, want the operator's BRW_AGENT_NAME to win", got)
	}
}

func TestLogicalOwnerIsStableAcrossDisposableProxyRestarts(t *testing.T) {
	t.Setenv("BRW_OWNER_ID", "")
	t.Setenv("MCPLEXER_BROWSER_SESSION_ID", "worker:agent-42")
	first, err := New("http://localhost:1234", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("http://localhost:1234", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID() == second.SessionID() {
		t.Fatal("disposable proxies unexpectedly share a correlation session")
	}
	if first.OwnerID() == "" || first.OwnerID() != second.OwnerID() {
		t.Fatalf("logical owner changed across proxy restart: %q != %q", first.OwnerID(), second.OwnerID())
	}
	if first.OwnerID() == "worker:agent-42" {
		t.Fatal("raw gateway session id must not be forwarded as the lease owner")
	}
}

func TestReplayRequestForwardsBodyWindowAcrossHTTP(t *testing.T) {
	var got struct {
		Offset   int `json:"offset"`
		MaxBytes int `json:"max_bytes"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/replay_request" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(snapshot.ReplayResult{OK: true, Body: "chunk", BodyOffset: got.Offset})
	}))
	defer srv.Close()
	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.ReplayRequest(context.Background(), browser.ReplayRequestParams{
		Method: "GET", URL: "https://example.test/data", Offset: 19_000, MaxBytes: 32_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != 19_000 || got.MaxBytes != 32_000 || result.BodyOffset != 19_000 {
		t.Fatalf("window lost across HTTP: request=%+v result=%+v", got, result)
	}
}

func TestReadWindowIsAppliedOnBrowserHost(t *testing.T) {
	const payloadChars = 1 << 20
	var requestedMaxChars string
	var wireBytes int
	full := readability.PageRead{
		URL: "https://synthetic.example.test/large", Title: "Synthetic large page",
		Main: strings.Repeat("x", payloadChars),
	}
	fullEncoded, _ := json.Marshal(full)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/read" {
			http.NotFound(w, r)
			return
		}
		requestedMaxChars = r.URL.Query().Get("max_chars")
		maxChars, _ := strconv.Atoi(requestedMaxChars)
		encoded, err := json.Marshal(readability.Window(full, readability.ReadOptions{MaxChars: maxChars}))
		if err != nil {
			t.Error(err)
			return
		}
		wireBytes = len(encoded)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer upstream.Close()

	controller, err := New(upstream.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	read, err := controller.ReadWindow(context.Background(), readability.ReadOptions{MaxChars: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	returned, _ := json.Marshal(read)
	if requestedMaxChars != "20000" || len(read.Main) != 20_000 || !read.MainTruncated {
		t.Fatalf("max=%q window=%d truncated=%v", requestedMaxChars, len(read.Main), read.MainTruncated)
	}
	if wireBytes > len(returned)+2 || len(fullEncoded) < wireBytes*40 {
		t.Fatalf("full=%d wire=%d returned=%d; bounds were not applied at the browser host", len(fullEncoded), wireBytes, len(returned))
	}
	t.Logf("browser-host window transferred %d bytes instead of %d (%.1fx less data)", wireBytes, len(fullEncoded), float64(len(fullEncoded))/float64(wireBytes))
}

func TestUpstreamResponseSizeIsBoundedBeforeReadingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(maxUpstreamResponseBytes+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	controller, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.ListTabs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 MiB") {
		t.Fatalf("oversized upstream response was not rejected: %v", err)
	}
}

func TestOpen_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/browser/open" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["url"] != "https://example.com" {
			t.Errorf("unexpected url: %s", req["url"])
		}
		json.NewEncoder(w).Encode(browser.OpenResult{
			Tab: browser.Tab{ID: "tab1", URL: "https://example.com", Title: "Example"},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Open(context.Background(), "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Tab.ID != "tab1" {
		t.Fatalf("expected tab1, got %q", result.Tab.ID)
	}
}

func TestOpenInGroup_ForwardsGroupOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/browser/open" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["url"] != "https://example.com" || req["group"] != "workspace-2" || req["group_id"] != "9" || req["group_color"] != "cyan" {
			t.Errorf("unexpected request body: %+v", req)
		}
		json.NewEncoder(w).Encode(browser.OpenResult{
			Tab: browser.Tab{ID: "tab1", URL: "https://example.com", GroupID: "9", GroupTitle: "workspace-2", GroupColor: "cyan"},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.OpenInGroup(context.Background(), "https://example.com", browser.TabGroupOptions{
		GroupID: "9",
		Name:    "workspace-2",
		Color:   "cyan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tab.GroupID != "9" || result.Tab.GroupTitle != "workspace-2" || result.Tab.GroupColor != "cyan" {
		t.Fatalf("unexpected grouped tab: %+v", result.Tab)
	}
}

func TestListTabs_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/browser/tabs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]browser.Tab{
			{ID: "tab1", URL: "https://a.com", Title: "A"},
			{ID: "tab2", URL: "https://b.com", Title: "B"},
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	tabs, err := c.ListTabs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 2 {
		t.Fatalf("expected 2 tabs, got %d", len(tabs))
	}
}

func TestListTabGroups_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/browser/tab_groups" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]browser.TabGroup{
			{ID: "9", Title: "workspace-2", Color: "cyan", TabIDs: []string{"tab1", "tab2"}, TabCount: 2},
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	groups, err := c.ListTabGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "9" || groups[0].TabCount != 2 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestGroupTabs_ForwardsGroupOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/browser/group_tabs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			TabIDs  []string `json:"tab_ids"`
			GroupID string   `json:"group_id"`
			Name    string   `json:"name"`
			Color   string   `json:"color"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.TabIDs) != 2 || req.TabIDs[0] != "41" || req.TabIDs[1] != "42" || req.GroupID != "9" || req.Name != "workspace-2" || req.Color != "cyan" {
			t.Errorf("unexpected request body: %+v", req)
		}
		json.NewEncoder(w).Encode(browser.ActionResult{OK: true})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	if err := c.GroupTabs(context.Background(), []string{"41", "42"}, browser.TabGroupOptions{
		GroupID: "9",
		Name:    "workspace-2",
		Color:   "cyan",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClick_ForwardsTabID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/page/click" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["ref"] != "e17" {
			t.Errorf("unexpected ref: %v", req["ref"])
		}
		json.NewEncoder(w).Encode(browser.ActionResult{OK: true, Message: "clicked e17"})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	result, err := c.Click(context.Background(), "e17")
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal("expected OK")
	}
}

func TestSnapshot_ForwardsOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/snapshot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("mode") != "all" {
			t.Errorf("expected mode=all, got %q", r.URL.Query().Get("mode"))
		}
		for name, want := range map[string]string{
			"text_content":         "true",
			"visual_islands":       "true",
			"visual_islands_limit": "6",
		} {
			if got := r.URL.Query().Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		json.NewEncoder(w).Encode(snapshot.PageSnapshot{
			URL:   "https://example.com",
			Title: "Example",
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	_, err := c.Snapshot(context.Background(), snapshot.SnapshotOptions{Mode: "all", TextContent: true, VisualIslands: true, VisualIslandsLimit: 6})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServerDown_ReturnsError(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", 100*time.Millisecond)
	_, err := c.Open(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "something broke"})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	_, err := c.Open(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestScreenshot_ReturnsBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("base64") != "1" {
			t.Errorf("expected base64=1 query param")
		}
		json.NewEncoder(w).Encode(browser.Screenshot{
			MIMEType: "image/png",
			Base64:   "ZmFrZS1wbmctZGF0YQ==",
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	shot, err := c.Screenshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if shot.MIMEType != "image/png" {
		t.Fatalf("unexpected mime type: %q", shot.MIMEType)
	}
	if shot.Base64 != "ZmFrZS1wbmctZGF0YQ==" {
		t.Fatalf("unexpected base64: %q", shot.Base64)
	}
	if string(shot.Data) != "fake-png-data" {
		t.Fatalf("unexpected decoded data: %q", shot.Data)
	}
}

func TestReadWindowPushesEveryBoundToBrowserHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/read" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		want := map[string]string{
			"max_chars": "123", "offset": "45", "max_links": "6", "max_headings": "7",
			"include": "main,headings", "section": "Invoices",
		}
		for name, expected := range want {
			if got := r.URL.Query().Get(name); got != expected {
				t.Errorf("%s=%q want %q", name, got, expected)
			}
		}
		_ = json.NewEncoder(w).Encode(readability.PageRead{Main: strings.Repeat("x", 123), MainTruncated: true, NextOffset: 168})
	}))
	defer srv.Close()
	c, _ := New(srv.URL, 5*time.Second)
	read, err := c.ReadWindow(context.Background(), readability.ReadOptions{
		MaxChars: 123, Offset: 45, MaxLinks: 6, MaxHeadings: 7,
		Include: []string{"main", "headings"}, Section: "Invoices",
	})
	if err != nil || len(read.Main) != 123 || read.NextOffset != 168 {
		t.Fatalf("read=%+v err=%v", read, err)
	}
}

func TestReadWindowSendsZeroToSelectHostDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"max_chars", "offset", "max_links", "max_headings"} {
			if got := r.URL.Query().Get(name); got != "0" {
				t.Errorf("%s=%q; zero must be explicit", name, got)
			}
		}
		_ = json.NewEncoder(w).Encode(readability.PageRead{})
	}))
	defer srv.Close()
	c, _ := New(srv.URL, 5*time.Second)
	if _, err := c.ReadWindow(context.Background(), readability.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
}
