package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/mcp"
)

type approvalProxyBrowser struct {
	browser.Controller
	mu             sync.Mutex
	state          string
	clicks         int
	headers        []string
	authorizations []string
}

func (c *approvalProxyBrowser) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "proxy-tab", URL: "https://proxy-approval.test/cart", Active: true}}, nil
}
func (c *approvalProxyBrowser) Evaluate(ctx context.Context, expression string) (any, error) {
	if browser.TabIDFromContext(ctx) != "proxy-tab" || !strings.Contains(expression, "time_origin:performance.timeOrigin") {
		return nil, fmt.Errorf("unexpected evidence call")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"url": "https://proxy-approval.test/cart", "complete": true, "controls": []any{map[string]any{"value": c.state}}, "preview": map[string]any{"title": "Cart"}}, nil
}
func (c *approvalProxyBrowser) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	if browser.TabIDFromContext(ctx) != "proxy-tab" || ref != "e1" || !approval.IsExecution(ctx) {
		return browser.ActionResult{}, fmt.Errorf("incorrectly pinned or unapproved click")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clicks++
	return browser.ActionResult{OK: true}, nil
}
func (c *approvalProxyBrowser) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.clicks }
func (c *approvalProxyBrowser) changeState(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = value
}

func newApprovalProxyIntegration(t *testing.T) (*Controller, *approval.Store, *approvalProxyBrowser) {
	t.Helper()
	host := &approvalProxyBrowser{state: "original"}
	store, err := approval.Open(filepath.Join(t.TempDir(), "private", "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate, err := approvalgate.New(host, store, "all")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.New("127.0.0.1:0", host)
	server.SetApprovalGate(gate)
	if err = server.SetApprovalStore(store, "test-human-only-operator-token-123456789"); err != nil {
		t.Fatal(err)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/page/click" {
			host.mu.Lock()
			host.headers = append(host.headers, r.Header.Get("X-Brw-Approval-Id"))
			host.authorizations = append(host.authorizations, r.Header.Get("Authorization"))
			host.mu.Unlock()
		}
		server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	gate.SetOperatorOrigin(daemon.URL)
	proxy, err := New(daemon.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proxy.sessionID = "approval-proxy-session"
	proxy.ownerID = "approval-proxy-owner"
	return proxy, store, host
}

func proxyApprovalRequired(t *testing.T, proxy *Controller, ctx context.Context) string {
	t.Helper()
	_, err := proxy.Click(ctx, "e1")
	var required *approvalgate.RequiredError
	if !errors.As(err, &required) || required.RequestID == "" || required.Status != approval.Pending || !strings.HasSuffix(required.StatusURL, "/api/approvals/"+required.RequestID) {
		t.Fatalf("proxy lost typed approval refusal or ID: %T %v", err, err)
	}
	return required.RequestID
}

func TestApprovalIntegrationHTTPProxyExactIDRetry(t *testing.T) {
	proxy, store, host := newApprovalProxyIntegration(t)
	ctx := browser.WithTabID(context.Background(), "proxy-tab")
	id := proxyApprovalRequired(t, proxy, ctx)
	if host.count() != 0 {
		t.Fatal("initial HTTP request executed")
	}
	request, _ := store.Get(id)
	if request.TabID != "proxy-tab" || request.SessionID != "approval-proxy-owner:approval-proxy-session" {
		t.Fatalf("proxy binding lost: %+v", request)
	}
	status, err := proxy.ApprovalStatus(ctx, id)
	if err != nil || status["status"] != approval.Pending || len(status) != 3 {
		t.Fatalf("public proxy status leaked or failed: %+v %v", status, err)
	}
	if _, err = store.Decide(id, approval.Approved, "operator", ""); err != nil {
		t.Fatal(err)
	}
	retry := approval.WithRequestID(ctx, id)
	result, err := proxy.Click(retry, "e1")
	if err != nil || !result.OK || host.count() != 1 {
		t.Fatalf("proxy exact retry did not click once: %+v %v count=%d", result, err, host.count())
	}
	_, err = proxy.Click(retry, "e1")
	var consumed *approvalgate.RequiredError
	if !errors.As(err, &consumed) || consumed.Status != approval.Consumed || host.count() != 1 {
		t.Fatalf("consumed proxy retry clicked: %T %v count=%d", err, err, host.count())
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.headers) != 3 || host.headers[0] != "" || host.headers[1] != id || host.headers[2] != id {
		t.Fatalf("approval ID not forwarded exactly: %v", host.headers)
	}
	for _, authorization := range host.authorizations {
		if authorization != "" {
			t.Fatal("proxy action forwarded operator credentials")
		}
	}
}

func TestApprovalIntegrationHTTPProxyChangedStateAndArguments(t *testing.T) {
	for _, kind := range []string{"page", "arguments", "session"} {
		t.Run(kind, func(t *testing.T) {
			proxy, store, host := newApprovalProxyIntegration(t)
			ctx := browser.WithTabID(context.Background(), "proxy-tab")
			id := proxyApprovalRequired(t, proxy, ctx)
			if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
				t.Fatal(err)
			}
			ref := "e1"
			wantCode := "binding_mismatch"
			switch kind {
			case "page":
				host.changeState("changed")
				wantCode = "stale"
			case "arguments":
				ref = "e2"
			case "session":
				proxy.sessionID = "different-session"
			}
			_, err := proxy.Click(approval.WithRequestID(ctx, id), ref)
			var refusal *approval.Error
			if !errors.As(err, &refusal) || refusal.Code != wantCode || refusal.RequestID != id || host.count() != 0 {
				t.Fatalf("changed %s allowed or lost typed error: %T %v count=%d", kind, err, err, host.count())
			}
			current, _ := store.Get(id)
			if kind == "page" && current.Status != approval.Stale {
				t.Fatalf("changed page not invalidated: %s", current.Status)
			}
		})
	}
}

func TestApprovalIntegrationHTTPProxyCannotDecideWithRequestID(t *testing.T) {
	proxy, store, host := newApprovalProxyIntegration(t)
	ctx := browser.WithTabID(context.Background(), "proxy-tab")
	id := proxyApprovalRequired(t, proxy, ctx)
	_, err := proxy.Request(approval.WithRequestID(ctx, id), http.MethodPost, "/operator/approvals/"+id+"/decision", nil, map[string]string{"decision": "approved"})
	if RemoteStatus(err) != http.StatusUnauthorized {
		t.Fatalf("agent ID obtained operator authority: %T %v", err, err)
	}
	current, _ := store.Get(id)
	if current.Status != approval.Pending || host.count() != 0 {
		t.Fatal("unprivileged decision changed approval or executed action")
	}
}

func TestApprovalIntegrationMCPOverHTTPProxyResume(t *testing.T) {
	proxy, store, host := newApprovalProxyIntegration(t)
	srv := mcp.New(proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, inReader, outWriter) }()
	stop := context.AfterFunc(ctx, func() { inReader.CloseWithError(ctx.Err()); outReader.CloseWithError(ctx.Err()) })
	defer stop()
	defer func() { cancel(); inWriter.Close(); outReader.Close(); <-done }()
	encoder := json.NewEncoder(inWriter)
	decoder := json.NewDecoder(outReader)
	call := func(id int, name string, args map[string]any) map[string]any {
		t.Helper()
		request := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		for {
			var response struct {
				ID     int            `json:"id"`
				Result map[string]any `json:"result"`
				Error  any            `json:"error"`
			}
			if err := decoder.Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.ID != id {
				continue
			}
			if response.Error != nil {
				t.Fatalf("MCP proxy call %s RPC error: %+v", name, response.Error)
			}
			return response.Result
		}
	}
	args := map[string]any{"ref": "e1", "tab_id": "proxy-tab"}
	result := call(1, "brw_click", args)
	detail, _ := result["structuredContent"].(map[string]any)
	id, _ := detail["approval_id"].(string)
	if result["isError"] != true || id == "" || detail["code"] != "approval_required" {
		t.Fatalf("MCP proxy lost approval requirement: %+v", result)
	}
	if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
		t.Fatal(err)
	}
	result = call(2, "brw_approval_resume", map[string]any{"approval_id": id, "tool": "brw_click", "arguments": args})
	if result["isError"] == true || host.count() != 1 {
		t.Fatalf("MCP proxy resume did not execute exactly once: %+v count=%d", result, host.count())
	}
}
