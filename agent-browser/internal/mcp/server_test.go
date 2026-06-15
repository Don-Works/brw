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

func TestBrowserSnapshotSchemaDocumentsVisualIslands(t *testing.T) {
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
	props := snapshotTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["visual_islands"]; !ok {
		t.Fatalf("visual_islands missing from snapshot schema: %#v", props)
	}
	if _, ok := props["visual_islands_limit"]; !ok {
		t.Fatalf("visual_islands_limit missing from snapshot schema: %#v", props)
	}
}

func TestBrowserScreenshotSchemaDocumentsAnnotate(t *testing.T) {
	var shotTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_screenshot" {
			shotTool = tool
			break
		}
	}
	if shotTool == nil {
		t.Fatal("browser_screenshot tool not found")
	}
	desc := shotTool["description"].(string)
	if !strings.Contains(desc, "annotate") || !strings.Contains(desc, "Set-of-Marks") {
		t.Fatalf("browser_screenshot description does not document annotate/Set-of-Marks: %q", desc)
	}
	props := shotTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["annotate"]; !ok {
		t.Fatalf("annotate missing from browser_screenshot schema: %#v", props)
	}
}

func TestBrowserScreenshotAnnotateReturnsLegend(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_screenshot","arguments":{"annotate":true}}}` + "\n"
	var output bytes.Buffer
	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	result := resp["result"].(map[string]any)
	legend, ok := result["legend"].(map[string]any)
	if !ok {
		t.Fatalf("annotated screenshot result missing legend: %#v", result)
	}
	e1, ok := legend["e1"].(map[string]any)
	if !ok {
		t.Fatalf("legend missing e1: %#v", legend)
	}
	if e1["role"] != "button" || e1["name"] != "Submit" {
		t.Fatalf("legend e1 = %#v", e1)
	}
	content := result["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" {
		t.Fatalf("annotated content[0] = %#v", img)
	}
}

func TestBrowserScreenshotDefaultUnchanged(t *testing.T) {
	// annotate omitted -> plain path, no legend.
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_screenshot","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	result := resp["result"].(map[string]any)
	if _, ok := result["legend"]; ok {
		t.Fatalf("plain screenshot must not include a legend: %#v", result)
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

func TestBrowserCancelDispatchesTokenAndReturnsStructured(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_cancel","arguments":{"token":"op-7"}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.cancelToken != "op-7" {
		t.Fatalf("cancel token = %q, want op-7", ctrl.cancelToken)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	structured := resp["result"].(map[string]any)["structuredContent"].(map[string]any)
	if structured["cancelled"].(float64) != 1 {
		t.Fatalf("cancelled = %v, want 1", structured["cancelled"])
	}
	if structured["token"] != "op-7" {
		t.Fatalf("token = %v, want op-7", structured["token"])
	}
}

func TestBrowserCancelToolSchemaDocumentsTokenAndTab(t *testing.T) {
	var cancelTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_cancel" {
			cancelTool = tool
			break
		}
	}
	if cancelTool == nil {
		t.Fatal("browser_cancel tool not registered")
	}
	props := cancelTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"token", "tab_id"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("browser_cancel schema missing %s: %#v", prop, props)
		}
	}
}

func TestBrowserNotifyDispatchesToController(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_notify","arguments":{"kind":"needs_input","title":"MFA required","message":"Enter your one-time code"}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.notifyOpts.Kind != "needs_input" || ctrl.notifyOpts.Title != "MFA required" || ctrl.notifyOpts.Message != "Enter your one-time code" {
		t.Fatalf("notify options = %#v", ctrl.notifyOpts)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	result := resp["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	if structured["ok"] != true || structured["delivery"] != "extension" {
		t.Fatalf("structured notify result = %#v", structured)
	}
}

func TestBrowserNotifyRegisteredInToolsList(t *testing.T) {
	var notifyTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_notify" {
			notifyTool = tool
			break
		}
	}
	if notifyTool == nil {
		t.Fatal("browser_notify tool not registered")
	}
	props := notifyTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"kind", "title", "message"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("browser_notify schema missing %s: %#v", prop, props)
		}
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

func TestBrowserNavigateToolRegistration(t *testing.T) {
	var navTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_navigate" {
			navTool = tool
			break
		}
	}
	if navTool == nil {
		t.Fatal("browser_navigate tool not found")
	}
	schema := navTool["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["direction"]; !ok {
		t.Fatalf("browser_navigate schema missing direction: %#v", props)
	}
	if _, ok := props["tab_id"]; !ok {
		t.Fatalf("browser_navigate schema missing tab_id: %#v", props)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "direction" {
		t.Fatalf("browser_navigate required = %#v, want [direction]", schema["required"])
	}
}

func TestBrowserNavigateDispatch(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_navigate","arguments":{"direction":"back"}}}` + "\n"
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.navigateDirection != "back" {
		t.Fatalf("navigate direction = %q, want back", ctrl.navigateDirection)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["result"].(map[string]any); !ok {
		t.Fatalf("missing result: %#v", resp)
	}
}

func TestFocusAndCloseTabAcceptTabIDAlias(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		args     string
		wantID   string
		focusTab bool
	}{
		{"focus_tab accepts tab_id", "browser_focus_tab", `{"tab_id":"42"}`, "42", true},
		{"focus_tab accepts legacy id", "browser_focus_tab", `{"id":"7"}`, "7", true},
		{"focus_tab prefers tab_id over id", "browser_focus_tab", `{"id":"7","tab_id":"42"}`, "42", true},
		{"close_tab accepts tab_id", "browser_close_tab", `{"tab_id":"99"}`, "99", false},
		{"close_tab accepts legacy id", "browser_close_tab", `{"id":"3"}`, "3", false},
		{"close_tab prefers tab_id over id", "browser_close_tab", `{"id":"3","tab_id":"99"}`, "99", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &recordingController{}
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}` + "\n"
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			got := ctrl.closeID
			if tc.focusTab {
				got = ctrl.focusID
			}
			if got != tc.wantID {
				t.Fatalf("dispatched id = %q, want %q (args %s)", got, tc.wantID, tc.args)
			}
		})
	}
}

func TestFocusAndCloseTabSchemasExposeTabIDAlias(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range tools() {
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{"browser_focus_tab", "browser_close_tab"} {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("%s tool not registered", name)
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, prop := range []string{"tab_id", "id"} {
			if _, ok := props[prop]; !ok {
				t.Fatalf("%s schema missing %s: %#v", name, prop, props)
			}
		}
	}
}

func TestBrowserClickTextPassesAutoScroll(t *testing.T) {
	cases := []struct {
		name string
		args string
		want *bool
	}{
		{"omitted leaves auto_scroll nil (script defaults true)", `{"text":"Alternative medicine"}`, nil},
		{"explicit false opts out", `{"text":"Alternative medicine","auto_scroll":false}`, boolPtr(false)},
		{"explicit true keeps it", `{"text":"Alternative medicine","auto_scroll":true}`, boolPtr(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &recordingController{}
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_click_text","arguments":` + tc.args + `}}` + "\n"
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			got := ctrl.clickTextOpts.AutoScroll
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("auto_scroll = %v, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("auto_scroll = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Fatalf("auto_scroll = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestBrowserEvaluateDescriptionDocumentsCSP(t *testing.T) {
	var evalTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "browser_evaluate" {
			evalTool = tool
			break
		}
	}
	if evalTool == nil {
		t.Fatal("browser_evaluate tool not registered")
	}
	desc := evalTool["description"].(string)
	if !strings.Contains(strings.ToLower(desc), "content-security-policy") && !strings.Contains(strings.ToUpper(desc), "CSP") {
		t.Fatalf("browser_evaluate description does not mention CSP: %q", desc)
	}
	if !strings.Contains(strings.ToLower(desc), "cross-origin") {
		t.Fatalf("browser_evaluate description does not mention cross-origin caveat: %q", desc)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestBrowserClickRoutesByButtonAndCount(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		wantPlain  string
		wantButton string
		wantCount  int
	}{
		{"plain left ref click stays on fast path", `{"ref":"e5"}`, "e5", "", 0},
		{"right click routes to ClickButton", `{"ref":"e5","button":"right"}`, "", "right", 0},
		{"double click routes to ClickButton", `{"ref":"e5","click_count":2}`, "", "", 2},
		{"coordinate click routes to ClickButton", `{"x":10,"y":20}`, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &recordingController{}
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_click","arguments":` + tc.args + `}}` + "\n"
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			if tc.wantPlain != "" {
				if ctrl.clickRef != tc.wantPlain {
					t.Fatalf("plain click ref = %q, want %q", ctrl.clickRef, tc.wantPlain)
				}
				if ctrl.clickButton.Ref != "" || ctrl.clickButton.X != nil {
					t.Fatalf("expected fast path, but ClickButton called: %#v", ctrl.clickButton)
				}
				return
			}
			if ctrl.clickRef != "" {
				t.Fatalf("expected ClickButton path, but fast Click called with %q", ctrl.clickRef)
			}
			if ctrl.clickButton.Button != tc.wantButton {
				t.Fatalf("click button = %q, want %q", ctrl.clickButton.Button, tc.wantButton)
			}
			if ctrl.clickButton.ClickCount != tc.wantCount {
				t.Fatalf("click count = %d, want %d", ctrl.clickButton.ClickCount, tc.wantCount)
			}
		})
	}
}

func TestBrowserDragAndMousePrimitivesDispatch(t *testing.T) {
	ctrl := &recordingController{}
	calls := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_drag","arguments":{"from":{"ref":"e1"},"to":{"x":200,"y":50},"steps":6,"button":"left"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"browser_mouse_down","arguments":{"ref":"e1","button":"left"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"browser_mouse_up","arguments":{"x":12,"y":34}}}`,
	}
	input := strings.Join(calls, "\n") + "\n"
	var output bytes.Buffer
	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if ctrl.dragOpts.From.Ref != "e1" || ctrl.dragOpts.To.X == nil || *ctrl.dragOpts.To.X != 200 || ctrl.dragOpts.Steps != 6 {
		t.Fatalf("drag opts = %#v", ctrl.dragOpts)
	}
	if ctrl.mouseDownOpt.Ref != "e1" || ctrl.mouseDownOpt.Button != "left" {
		t.Fatalf("mouse_down opts = %#v", ctrl.mouseDownOpt)
	}
	if ctrl.mouseUpOpt.X == nil || *ctrl.mouseUpOpt.X != 12 || ctrl.mouseUpOpt.Y == nil || *ctrl.mouseUpOpt.Y != 34 {
		t.Fatalf("mouse_up opts = %#v", ctrl.mouseUpOpt)
	}
}

func TestMouseToolSchemasRegistered(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range tools() {
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{"browser_drag", "browser_mouse_down", "browser_mouse_up"} {
		if byName[name] == nil {
			t.Fatalf("%s tool not registered", name)
		}
	}
	clickProps := byName["browser_click"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"button", "click_count", "x", "y"} {
		if _, ok := clickProps[prop]; !ok {
			t.Fatalf("browser_click schema missing %s: %#v", prop, clickProps)
		}
	}
	dragProps := byName["browser_drag"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"from", "to", "steps", "button"} {
		if _, ok := dragProps[prop]; !ok {
			t.Fatalf("browser_drag schema missing %s: %#v", prop, dragProps)
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
	snapshotOpts      snapshot.SnapshotOptions
	findOpts          snapshot.FindOptions
	navigateDirection string
	cancelToken       string
	notifyOpts        browser.NotifyOptions
	clickRef          string
	clickButton       browser.ClickButtonOptions
	dragOpts          browser.DragOptions
	mouseDownOpt      browser.MouseButtonOptions
	mouseUpOpt        browser.MouseButtonOptions
	focusID           string
	closeID           string
	clickTextOpts     snapshot.ClickTextOptions
}

func (r *recordingController) FocusTab(ctx context.Context, id string) error {
	r.focusID = id
	return nil
}

func (r *recordingController) CloseTab(ctx context.Context, id string) error {
	r.closeID = id
	return nil
}

func (r *recordingController) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	r.clickTextOpts = opts
	return browser.ActionResult{OK: true}, nil
}

func (r *recordingController) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	r.navigateDirection = direction
	return r.fakeController.Navigate(ctx, direction)
}

func (r *recordingController) Cancel(ctx context.Context, token string) (browser.CancelResult, error) {
	r.cancelToken = token
	return browser.CancelResult{OK: true, Token: token, Cancelled: 1}, nil
}

func (r *recordingController) Notify(ctx context.Context, opts browser.NotifyOptions) (browser.NotifyResult, error) {
	r.notifyOpts = opts
	return browser.NotifyResult{OK: true, Delivery: "extension"}, nil
}

func (r *recordingController) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	r.clickRef = ref
	return r.fakeController.Click(ctx, ref)
}

func (r *recordingController) ClickButton(ctx context.Context, opts browser.ClickButtonOptions) (browser.ActionResult, error) {
	r.clickButton = opts
	return r.fakeController.ClickButton(ctx, opts)
}

func (r *recordingController) Drag(ctx context.Context, opts browser.DragOptions) (browser.ActionResult, error) {
	r.dragOpts = opts
	return r.fakeController.Drag(ctx, opts)
}

func (r *recordingController) MouseDown(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	r.mouseDownOpt = opts
	return r.fakeController.MouseDown(ctx, opts)
}

func (r *recordingController) MouseUp(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	r.mouseUpOpt = opts
	return r.fakeController.MouseUp(ctx, opts)
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
func (fakeController) OpenIncognito(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "t1", BrowserContextID: "ctx-1"}}, nil
}
func (fakeController) CloseContext(context.Context, string) error      { return nil }
func (fakeController) ListTabs(context.Context) ([]browser.Tab, error) { return nil, nil }
func (fakeController) FocusTab(context.Context, string) error          { return nil }
func (fakeController) CloseTab(context.Context, string) error          { return nil }
func (fakeController) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{}, nil
}
func (fakeController) ReadData(context.Context) (snapshot.StructuredData, error) {
	return snapshot.StructuredData{}, nil
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
func (fakeController) Navigate(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) ClickButton(context.Context, browser.ClickButtonOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) MouseDown(context.Context, browser.MouseButtonOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) MouseUp(context.Context, browser.MouseButtonOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}
func (fakeController) Drag(context.Context, browser.DragOptions) (browser.ActionResult, error) {
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
	return browser.Screenshot{MIMEType: "image/png", Base64: "UExBSU4="}, nil
}
func (fakeController) ScreenshotAnnotated(context.Context, string) (browser.AnnotatedScreenshot, error) {
	return browser.AnnotatedScreenshot{
		MIMEType: "image/png",
		Base64:   "QU5OT1RBVEVE",
		Legend: map[string]browser.LegendEntry{
			"e1": {Ref: "e1", Name: "Submit", Role: "button", X: 10, Y: 20, Width: 100, Height: 40},
		},
	}, nil
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
func (fakeController) NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error) {
	return nil, nil
}
func (fakeController) ReplayRequest(context.Context, browser.ReplayRequestParams) (snapshot.ReplayResult, error) {
	return snapshot.ReplayResult{}, nil
}
func (fakeController) ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error) {
	return browser.PlanResult{}, nil
}
func (fakeController) ExecuteBatch(context.Context, []browser.BatchStep) (browser.BatchResult, error) {
	return browser.BatchResult{}, nil
}
func (fakeController) Cancel(context.Context, string) (browser.CancelResult, error) {
	return browser.CancelResult{OK: true}, nil
}
func (fakeController) Observe(context.Context) (browser.ObserveResult, error) {
	return browser.ObserveResult{}, nil
}
func (fakeController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return nil, nil
}
func (fakeController) Downloads(context.Context) (browser.DownloadsResult, error) {
	return browser.DownloadsResult{Downloads: []browser.DownloadEntry{}}, nil
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
func (fakeController) Notify(context.Context, browser.NotifyOptions) (browser.NotifyResult, error) {
	return browser.NotifyResult{OK: true, Delivery: "extension"}, nil
}
