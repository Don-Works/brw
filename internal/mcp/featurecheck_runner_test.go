package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/brwidentity"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/testbed"
)

type featureCase struct {
	Name  string
	Tools []string
	Run   func(*testing.T, *featureHarness)
}

var featureCases []featureCase

func registerFeatureCase(name string, tools []string, run func(*testing.T, *featureHarness)) {
	featureCases = append(featureCases, featureCase{name, tools, run})
}

type featureResult struct {
	Envelope map[string]any
	Data     map[string]any
	Text     string
	IsError  bool
}

type featureHarness struct {
	Ctx           context.Context
	Server        *mcp.Server
	Manager       *browser.Manager
	Fixture       *testbed.Server
	TabID         string
	startupTabs   map[string]bool
	Consent       *siteconsent.Guard
	Approvals     *approval.Store
	Root          string
	Operator      *httptest.Server
	called        map[string]int
	checked       map[string]int
	status        map[string]string
	rpcWriter     io.Writer
	rpcResponses  chan map[string]any
	rpcID         int
	rpcPending    map[int]map[string]any
	rpcStop       func()
	notifyMu      sync.Mutex
	notifications []map[string]any
}

func TestFeatureCatalogueCoverage(t *testing.T) {
	owners := map[string]string{}
	for _, c := range featureCases {
		if c.Name == "" || c.Run == nil || len(c.Tools) == 0 {
			t.Fatalf("invalid feature case: %+v", c)
		}
		for _, tool := range c.Tools {
			if old := owners[tool]; old != "" {
				t.Errorf("%s registered by both %s and %s", tool, old, c.Name)
			}
			owners[tool] = c.Name
		}
	}
	for _, tool := range featureToolNames() {
		if owners[tool] == "" {
			t.Errorf("unexercised feature: %s", tool)
		}
		delete(owners, tool)
	}
	for tool := range owners {
		t.Errorf("case names nonexistent tool: %s", tool)
	}
	t.Logf("catalogue: %d tools; %d executable scenarios", len(featureToolNames()), len(featureCases))
}

func featureToolNames() []string {
	names := mcp.ToolNames()
	for _, extra := range []string{"brw_skill", "brw_tools"} {
		if !slices.Contains(names, extra) {
			names = append(names, extra)
		}
	}
	slices.Sort(names)
	return names
}

func TestFeatureExercises(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable Chrome feature checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	fixture, err := testbed.Start(testbed.Config{Seed: 7, Chaos: 2, MaxEvents: 64, FrameAddress: "[::1]:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	profile := browsertest.NewProfile(t)
	manager, err := browser.New(ctx, browser.Config{UserDataDir: profile.Dir(), Headless: true, WebMCP: true, Timeout: 20 * time.Second, ChromeArgs: []string{"--disable-gpu", "--site-per-process", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--no-first-run", "--no-default-browser-check", "--window-size=1280,800", "--force-device-scale-factor=1", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE ::1"}})
	if err != nil {
		t.Fatal(err)
	}
	profile.StopWith(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	h := &featureHarness{Ctx: ctx, Manager: manager, Fixture: fixture, Root: t.TempDir()}
	rows := []map[string]any{}
	for _, c := range featureCases {
		if group := os.Getenv("BRW_FEATURE_CASE"); group != "" && !strings.Contains(c.Name, group) {
			continue
		}
		started := time.Now()
		passed := t.Run(c.Name, func(t *testing.T) {
			h.reset(t)
			c.Run(t, h)
			for _, tool := range c.Tools {
				if h.called[tool] == 0 || h.checked[tool] == 0 {
					t.Errorf("%s lacks an executed observable assertion (calls=%d checks=%d)", tool, h.called[tool], h.checked[tool])
				}
			}
		})
		for _, tool := range c.Tools {
			status := h.status[tool]
			if !passed || h.called[tool] == 0 || h.checked[tool] == 0 {
				status = "fail"
			}
			rows = append(rows, map[string]any{"tool": tool, "scenario": c.Name, "status": status, "calls": h.called[tool], "assertions": h.checked[tool], "duration_ms": time.Since(started).Milliseconds(), "transport": "direct-cdp"})
		}
	}
	if len(rows) == 0 {
		t.Fatal("no feature scenarios executed")
	}
	report := map[string]any{"schema_version": 1, "catalogue_tools": featureToolNames(), "features": rows, "billing_tokens": nil, "measurement": "fixture bytes and characters are distinct from provider billing"}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("BRW_FEATURE_REPORT"); output != "" {
		if err := os.WriteFile(output, data, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log(string(data))
	}
}

func (h *featureHarness) reset(t *testing.T) {
	t.Helper()
	h.called, h.checked, h.status = map[string]int{}, map[string]int{}, map[string]string{}
	h.fixtureRequest(t, "/api/reset", map[string]any{"seed": 7, "chaos": 2, "max_events": 64}, nil)
	tabs, err := h.Manager.ListTabs(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.startupTabs == nil {
		h.startupTabs = map[string]bool{}
		for _, tab := range tabs {
			if tab.Type == "page" {
				h.startupTabs[tab.ID] = true
			}
		}
	}
	for _, tab := range tabs {
		if tab.Type == "page" && !h.startupTabs[tab.ID] {
			_ = h.Manager.CloseTab(h.Ctx, tab.ID)
		}
	}
	remaining, err := h.Manager.ListTabs(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, tab := range remaining {
		present[tab.ID] = true
	}
	for id := range h.startupTabs {
		if !present[id] {
			t.Fatalf("fixture reset closed startup browser target %s", id)
		}
	}
	root, err := os.MkdirTemp(h.Root, "case-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := siteconsent.NewStoreWithKey(filepath.Join(root, "site-grants.json"), []byte("owned-fixture-consent-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	h.Consent, err = siteconsent.NewGuard(store, siteconsent.AdminConfig{ConfirmActions: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{h.Fixture.URL(), h.Fixture.FrameURL()} {
		for _, scope := range []siteconsent.Scope{siteconsent.ScopeRead, siteconsent.ScopeAct} {
			if _, err := h.Consent.Allow(siteconsent.GrantOptions{Origin: origin, Scope: scope, Actor: "owned-fixture-test"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.Approvals, err = approval.Open(filepath.Join(root, "approvals.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Approvals.Close() })
	h.Server = mcp.NewWithToolProfile(h.Manager, "all")
	h.Server.SetIdentity(brwidentity.Identity{Workspace: root, Profile: "owned-fixture", Mode: "headless", Transport: brwidentity.TransportDirectCDP, Headless: true})
	h.Server.SetSiteConsent(h.Consent)
	gate, err := approvalgate.New(h.Manager, h.Approvals, "risky")
	if err != nil {
		t.Fatal(err)
	}
	h.Server.SetApprovalGate(gate)
	operator := httpapi.New("127.0.0.1:0", h.Manager)
	if err := operator.SetApprovalStore(h.Approvals, featureOperatorToken); err != nil {
		t.Fatal(err)
	}
	h.Operator = httptest.NewServer(operator.Handler())
	t.Cleanup(h.Operator.Close)
	gate.SetOperatorOrigin(h.Operator.URL)
	h.startRPC(t)
	h.RPC(t, "initialize", map[string]any{"clientInfo": map[string]any{"name": "owned-feature-check"}})
	identity := h.OK(t, "brw_identity", nil)
	if identity.Data["connected"] != true {
		t.Fatal("owned browser identity is disconnected")
	}
	opened := h.OK(t, "brw_open", map[string]any{"url": h.Fixture.URL() + "/?frames=none"})
	tab, _ := opened.Data["tab"].(map[string]any)
	h.TabID, _ = tab["id"].(string)
	if h.TabID == "" {
		t.Fatalf("open has no tab identity: %s", opened.Text)
	}
	h.Wait(t, func(state testbed.State) bool { return state.SSEConnectionCount > 0 && state.WSConnectionCount > 0 })
}

const featureOperatorToken = "owned-fixture-operator-token-32characters"

func featureTabPinned(tool string) bool {
	return !slices.Contains([]string{"brw_identity", "brw_skill", "brw_tools", "brw_open", "brw_open_incognito", "brw_focus_tab", "brw_close_tab", "brw_list_tabs", "brw_list_tab_groups", "brw_close_context", "brw_group_tabs", "brw_ungroup_tabs", "brw_approval_status", "brw_approval_resume", "brw_watch_page", "brw_page_watchers", "brw_page_events", "brw_artifact_info", "brw_artifact_read", "brw_artifact_search", "brw_artifact_delete", "brw_recipe_search", "brw_cancel", "brw_state", "brw_set_download_path", "brw_read_url"}, tool)
}

func (h *featureHarness) startRPC(t *testing.T) {
	t.Helper()
	input, writer := io.Pipe()
	reader, output := io.Pipe()
	ctx, cancel := context.WithCancel(h.Ctx)
	h.rpcWriter = writer
	responses := make(chan map[string]any, 16)
	h.rpcResponses = responses
	h.rpcID = 0
	h.rpcPending = map[int]map[string]any{}
	h.notifyMu.Lock()
	h.notifications = nil
	h.notifyMu.Unlock()
	done := make(chan error, 1)
	go func() { done <- h.Server.Serve(ctx, input, output); _ = output.Close() }()
	go func() {
		decoder := json.NewDecoder(reader)
		for {
			var response map[string]any
			if decoder.Decode(&response) != nil {
				return
			}
			if _, ok := response["id"]; ok {
				select {
				case responses <- response:
				case <-ctx.Done():
					return
				}
			} else {
				h.notifyMu.Lock()
				h.notifications = append(h.notifications, response)
				h.notifyMu.Unlock()
			}
		}
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			_ = writer.Close()
			_ = reader.Close()
			select {
			case err := <-done:
				if err != nil && err != context.Canceled {
					t.Errorf("MCP shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("MCP shutdown timed out")
			}
		})
	}
	h.rpcStop = stop
	t.Cleanup(stop)
}

func (h *featureHarness) RPC(t *testing.T, method string, params any) map[string]any {
	return h.AwaitRPC(t, h.StartRPC(t, method, params))
}

func (h *featureHarness) StartRPC(t *testing.T, method string, params any) int {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if method == "tools/call" {
		var call struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &call) == nil {
			h.called[call.Name]++
		}
	}
	return h.startRequest(t, method, data)
}

func (h *featureHarness) UseServer(t *testing.T, server *mcp.Server) {
	t.Helper()
	if h.rpcStop != nil {
		h.rpcStop()
	}
	h.Server = server
	h.Server.SetIdentity(brwidentity.Identity{Workspace: h.Root, Profile: "owned-fixture", Mode: "headless", Transport: brwidentity.TransportDirectCDP, Headless: true})
	h.Server.SetSiteConsent(h.Consent)
	gate, err := approvalgate.New(h.Manager, h.Approvals, "risky")
	if err != nil {
		t.Fatal(err)
	}
	gate.SetOperatorOrigin(h.Operator.URL)
	h.Server.SetApprovalGate(gate)
	h.startRPC(t)
	h.RPC(t, "initialize", map[string]any{"clientInfo": map[string]any{"name": "owned-feature-check"}})
	h.OK(t, "brw_identity", nil)
}

func (h *featureHarness) Send(t *testing.T, method string, params any) {
	t.Helper()
	if err := json.NewEncoder(h.rpcWriter).Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
}

func (h *featureHarness) rpc(t *testing.T, method string, params json.RawMessage) map[string]any {
	return h.AwaitRPC(t, h.startRequest(t, method, params))
}

func (h *featureHarness) startRequest(t *testing.T, method string, params json.RawMessage) int {
	t.Helper()
	h.rpcID++
	if err := json.NewEncoder(h.rpcWriter).Encode(map[string]any{"jsonrpc": "2.0", "id": h.rpcID, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	return h.rpcID
}

func (h *featureHarness) AwaitRPC(t *testing.T, id int) map[string]any {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		if response := h.rpcPending[id]; response != nil {
			delete(h.rpcPending, id)
			if response["error"] != nil {
				t.Fatalf("RPC %d error: %+v", id, response["error"])
			}
			value, _ := response["result"].(map[string]any)
			return value
		}
		select {
		case response := <-h.rpcResponses:
			responseID, ok := response["id"].(float64)
			if !ok || responseID < 1 || responseID > float64(h.rpcID) {
				t.Fatalf("unexpected RPC id: %+v", response)
			}
			h.rpcPending[int(responseID)] = response
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		case <-timer.C:
			t.Fatalf("RPC %d response timed out", id)
		}
	}
}

func (h *featureHarness) RequireApprovals(t *testing.T) {
	t.Helper()
	gate, err := approvalgate.New(h.Manager, h.Approvals, "all")
	if err != nil {
		t.Fatal(err)
	}
	gate.SetOperatorOrigin(h.Operator.URL)
	h.Server.SetApprovalGate(gate)
}

func (h *featureHarness) Notifications() []map[string]any {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return slices.Clone(h.notifications)
}

func (h *featureHarness) Call(t *testing.T, tool string, args map[string]any) featureResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	copyArgs := make(map[string]any, len(args)+1)
	for k, v := range args {
		copyArgs[k] = v
	}
	if featureTabPinned(tool) && h.TabID != "" && copyArgs["tab_id"] == nil {
		copyArgs["tab_id"] = h.TabID
	}
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": copyArgs})
	if err != nil {
		t.Fatal(err)
	}
	value := h.rpc(t, "tools/call", params)
	h.called[tool]++
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	r := featureResult{}
	if err := json.Unmarshal(encoded, &r.Envelope); err != nil {
		t.Fatal(err)
	}
	r.IsError = r.Envelope["isError"] == true
	r.Data, _ = r.Envelope["structuredContent"].(map[string]any)
	if blocks, ok := r.Envelope["content"].([]any); ok {
		for _, block := range blocks {
			if b, ok := block.(map[string]any); ok {
				if text, ok := b["text"].(string); ok {
					r.Text += text
				}
			}
		}
	}
	if r.Data == nil {
		_ = json.Unmarshal([]byte(r.Text), &r.Data)
	}
	return r
}

func (h *featureHarness) OK(t *testing.T, tool string, args map[string]any) featureResult {
	t.Helper()
	r := h.Call(t, tool, args)
	if r.IsError && r.Data["code"] == "approval_required" {
		r = h.approveResult(t, tool, args, r)
	}
	if r.IsError {
		t.Fatalf("%s refused: %s", tool, r.Text)
	}
	return r
}

func (h *featureHarness) Check(t *testing.T, tool string, condition bool, format string, args ...any) {
	t.Helper()
	if h.called[tool] == 0 {
		t.Fatalf("assertion for uncalled tool %s", tool)
	}
	h.checked[tool]++
	h.status[tool] = "pass"
	if !condition {
		t.Fatalf("%s: %s", tool, fmt.Sprintf(format, args...))
	}
}

func (h *featureHarness) Unsupported(t *testing.T, tool string, result featureResult, capability string) {
	t.Helper()
	h.Check(t, tool, result.IsError && strings.Contains(result.Text, capability), "expected %s capability refusal, got %s", capability, result.Text)
	h.status[tool] = "unsupported"
}

func (h *featureHarness) State(t *testing.T) testbed.State {
	t.Helper()
	var state testbed.State
	h.fixtureRequest(t, "/api/state", nil, &state)
	return state
}

func (h *featureHarness) Event(t *testing.T, kind string) testbed.State {
	t.Helper()
	h.fixtureRequest(t, "/api/step", map[string]any{"run_id": h.State(t).RunID, "count": 1, "kind": kind}, nil)
	return h.Wait(t, func(s testbed.State) bool { return s.AppliedCursor == s.Cursor && s.AcknowledgedCursor == s.Cursor })
}

func (h *featureHarness) Wait(t *testing.T, predicate func(testbed.State) bool) testbed.State {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		state := h.State(t)
		if predicate(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture oracle timed out: cursor=%d applied=%d ack=%d", state.Cursor, state.AppliedCursor, state.AcknowledgedCursor)
		}
		select {
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (h *featureHarness) fixtureRequest(t *testing.T, path string, body any, out any) {
	t.Helper()
	method := http.MethodGet
	var data []byte
	var err error
	if body != nil {
		method = http.MethodPost
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(h.Ctx, method, h.Fixture.URL()+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("fixture %s: %d %s", path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *featureHarness) Ref(t *testing.T, query string) string {
	t.Helper()
	r := h.OK(t, "brw_find", map[string]any{"query": query, "limit": 20})
	elements, _ := r.Data["elements"].([]any)
	var controls []any
	for _, value := range elements {
		if e, ok := value.(map[string]any); ok && e["role"] != "label" {
			controls = append(controls, value)
		}
	}
	elements = controls
	var exact []any
	for _, value := range elements {
		if e, ok := value.(map[string]any); ok && e["name"] == query && e["role"] != "label" {
			exact = append(exact, value)
		}
	}
	if len(exact) == 1 {
		elements = exact
	}
	if len(elements) != 1 {
		t.Fatalf("%q resolves to %d controls: %s", query, len(elements), r.Text)
	}
	e, _ := elements[0].(map[string]any)
	ref, _ := e["ref"].(string)
	if ref == "" {
		t.Fatalf("%q has no observed ref", query)
	}
	return ref
}

func (h *featureHarness) Approved(t *testing.T, tool string, args map[string]any) featureResult {
	t.Helper()
	argsCopy := map[string]any{}
	for k, v := range args {
		argsCopy[k] = v
	}
	if featureTabPinned(tool) && argsCopy["tab_id"] == nil {
		argsCopy["tab_id"] = h.TabID
	}
	r := h.Call(t, tool, argsCopy)
	return h.approveResult(t, tool, argsCopy, r)
}

func (h *featureHarness) approveResult(t *testing.T, tool string, args map[string]any, r featureResult) featureResult {
	t.Helper()
	argsCopy := map[string]any{}
	for k, v := range args {
		argsCopy[k] = v
	}
	if featureTabPinned(tool) && argsCopy["tab_id"] == nil {
		argsCopy["tab_id"] = h.TabID
	}
	id, _ := r.Data["approval_id"].(string)
	if !r.IsError || r.Data["code"] != "approval_required" || id == "" {
		t.Fatalf("%s did not require actual approval: %s", tool, r.Text)
	}
	data := []byte(`{"decision":"approved","note":"Owned deterministic fixture only"}`)
	req, err := http.NewRequestWithContext(h.Ctx, http.MethodPost, h.Operator.URL+"/operator/approvals/"+id+"/decision", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+featureOperatorToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owned operator approval: HTTP%d", resp.StatusCode)
	}
	return h.OK(t, "brw_approval_resume", map[string]any{"approval_id": id, "tool": tool, "arguments": argsCopy})
}
