package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

func TestServeSupportsFramedStdio(t *testing.T) {
	input := framedJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	}) + framedJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	var output bytes.Buffer

	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	responses := bufio.NewReader(bytes.NewReader(output.Bytes()))
	first := readFramedResponse(t, responses)
	if first["id"].(float64) != 1 {
		t.Fatalf("first response id = %v", first["id"])
	}
	result := first["result"].(map[string]any)
	if result["serverInfo"].(map[string]any)["name"] != "agent-browserd" {
		t.Fatalf("unexpected initialize result: %#v", result)
	}

	second := readFramedResponse(t, responses)
	tools := second["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
}

func TestServeSupportsLineDelimitedJSON(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	var output bytes.Buffer

	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["id"].(float64) != 1 {
		t.Fatalf("response id = %v", resp["id"])
	}
}

func TestToolCallReturnsCompactTextAndStructuredContent(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_snapshot","arguments":{"query":"Continue","limit":1}}}` + "\n"
	var output bytes.Buffer

	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "\n") || strings.Contains(text, "  ") {
		t.Fatalf("tool text is not compact JSON: %q", text)
	}
	if _, ok := result["structuredContent"].(map[string]any); !ok {
		t.Fatalf("missing structuredContent: %#v", result)
	}
}

func TestBrowserSnapshotDefaultsToBoundedFrontier(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_snapshot","arguments":{}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.snapshotOpts.Mode != defaultSnapshotMode {
		t.Fatalf("mode = %q, want %q", ctrl.snapshotOpts.Mode, defaultSnapshotMode)
	}
	if ctrl.snapshotOpts.Limit != defaultSnapshotLimit {
		t.Fatalf("limit = %d, want %d", ctrl.snapshotOpts.Limit, defaultSnapshotLimit)
	}
	if !ctrl.snapshotOpts.ViewportOnly {
		t.Fatal("viewport_only = false, want true for default frontier mode")
	}
}

func TestBrowserSnapshotModeAllPreservesExplicitFullInspection(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_snapshot","arguments":{"mode":"all","include_hidden":true}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.snapshotOpts.Mode != "all" {
		t.Fatalf("mode = %q, want all", ctrl.snapshotOpts.Mode)
	}
	if ctrl.snapshotOpts.Limit != 0 {
		t.Fatalf("limit = %d, want 0 for explicit all mode", ctrl.snapshotOpts.Limit)
	}
	if ctrl.snapshotOpts.ViewportOnly {
		t.Fatal("viewport_only = true, want false for explicit all mode")
	}
	if !ctrl.snapshotOpts.IncludeHidden {
		t.Fatal("include_hidden = false, want true when explicitly requested")
	}
}

func TestBrowserSnapshotSchemaDocumentsDebugEscalation(t *testing.T) {
	var snapshotTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_snapshot" {
			snapshotTool = tool
			break
		}
	}
	if snapshotTool == nil {
		t.Fatal("browser_snapshot tool not found")
	}
	description := snapshotTool["description"].(string)
	if !strings.Contains(description, `mode:"all"`) || !strings.Contains(description, "include_hidden:true") {
		t.Fatalf("snapshot description does not document debug escalation: %q", description)
	}
	props := snapshotTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["include_hidden"]; !ok {
		t.Fatalf("include_hidden missing from snapshot schema: %#v", props)
	}
}

func TestBrowserFindDefaultsToBoundedResults(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_find","arguments":{"query":"checkout"}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.findOpts.Limit != defaultFindLimit {
		t.Fatalf("find limit = %d, want %d", ctrl.findOpts.Limit, defaultFindLimit)
	}
}

func TestToolSchemasExposeTabScopedErgonomics(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range tools() {
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{
		"browser_find",
		"browser_fill",
		"browser_select",
		"browser_press",
		"browser_scroll",
		"browser_wait_for",
		"browser_evaluate",
		"browser_click_xy",
		"browser_console",
	} {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("%s tool not found", name)
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["tab_id"]; !ok {
			t.Fatalf("%s schema missing tab_id: %#v", name, props)
		}
	}
	clickText := byName["browser_click_text"]
	if clickText == nil {
		t.Fatal("browser_click_text tool not found")
	}
	props := clickText["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"text", "role", "exact", "tab_id"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("browser_click_text missing %s: %#v", prop, props)
		}
	}
}

func framedJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data)
}

func readFramedResponse(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(line)
	_, rawLen, ok := strings.Cut(line, ":")
	if !ok {
		t.Fatalf("missing content-length header: %q", line)
	}
	length, err := strconv.Atoi(strings.TrimSpace(rawLen))
	if err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "" {
		t.Fatalf("expected blank separator, got %q", line)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

type fakeController struct{}

type recordingController struct {
	fakeController
	snapshotOpts snapshot.SnapshotOptions
	findOpts     snapshot.FindOptions
}

func (r *recordingController) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	r.snapshotOpts = opts
	return r.fakeController.Snapshot(ctx, opts)
}

func (r *recordingController) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	r.findOpts = opts
	return r.fakeController.Find(ctx, opts)
}

func (fakeController) Open(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}
func (fakeController) ListTabs(context.Context) ([]browser.Tab, error) { return nil, nil }
func (fakeController) FocusTab(context.Context, string) error          { return nil }
func (fakeController) CloseTab(context.Context, string) error          { return nil }
func (fakeController) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{}, nil
}
func (fakeController) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return snapshot.PageSnapshot{
		URL:   "https://example.com",
		Title: "Example",
		Elements: []snapshot.Element{{
			Ref:        "e1",
			Role:       "button",
			Name:       "Continue",
			Tag:        "button",
			Visible:    true,
			InViewport: true,
			Source:     []string{"dom"},
		}},
	}, nil
}
func (fakeController) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{}, nil
}
func (fakeController) Click(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) ClickText(context.Context, snapshot.ClickTextOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Type(context.Context, string, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Fill(context.Context, snapshot.FillOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) UploadFile(context.Context, snapshot.UploadOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Select(context.Context, string, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Press(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Scroll(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Screenshot(context.Context) (browser.Screenshot, error) {
	return browser.Screenshot{}, nil
}
func (fakeController) ScreenshotElement(context.Context, string) (browser.Screenshot, error) {
	return browser.Screenshot{}, nil
}
func (fakeController) WaitFor(context.Context, string, time.Duration) error { return nil }
func (fakeController) Hover(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Evaluate(context.Context, string) (any, error) { return nil, nil }
func (fakeController) NetworkRequests(context.Context, string) ([]browser.NetworkRequest, error) {
	return nil, nil
}
func (fakeController) ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error) {
	return browser.PlanResult{}, nil
}
func (fakeController) ExecuteBatch(context.Context, []browser.BatchStep) (browser.BatchResult, error) {
	return browser.BatchResult{}, nil
}
func (fakeController) Observe(context.Context) (browser.ObserveResult, error) {
	return browser.ObserveResult{}, nil
}
func (fakeController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return nil, nil
}
func (fakeController) ClickXY(context.Context, float64, float64) (snapshot.ClickXYResult, error) {
	return snapshot.ClickXYResult{}, nil
}
func (fakeController) GetTrace() browser.TraceResult { return browser.TraceResult{} }
func (fakeController) ClearTrace()                   {}
func (fakeController) OpenInGroup(context.Context, string, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}
func (fakeController) GroupTabs(context.Context, []string, string, string) error  { return nil }
func (fakeController) UngroupTabs(context.Context, []string) error                { return nil }
func (fakeController) AssertVisible(context.Context, string, time.Duration) error { return nil }
func (fakeController) AssertText(context.Context, string, string, time.Duration) error {
	return nil
}
func (fakeController) AssertValue(context.Context, string, string, time.Duration) error {
	return nil
}
func (fakeController) AssertHidden(context.Context, string, time.Duration) error { return nil }
func (fakeController) CommitField(context.Context, string) error                 { return nil }
