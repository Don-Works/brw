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

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
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
	if result["serverInfo"].(map[string]any)["name"] != "brw" {
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
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_snapshot","arguments":{"query":"Continue","limit":1}}}` + "\n"
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

func TestLegacyBrowserToolAliasStillCallsBRWTool(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_snapshot","arguments":{"query":"Continue","limit":1}}}` + "\n"
	var output bytes.Buffer

	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != nil {
		t.Fatalf("legacy browser_snapshot alias failed: %#v", resp["error"])
	}
}

func TestBrowserSnapshotDefaultsToBoundedFrontier(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_snapshot","arguments":{}}}` + "\n"
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

func TestBrowserOpenForwardsTabGroupOptions(t *testing.T) {
	ctrl := &recordingController{}
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "brw_open",
			"arguments": map[string]any{
				"url":         "https://example.com",
				"group":       "workspace-2",
				"group_id":    "9",
				"group_color": "cyan",
			},
		},
	})
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.openURL != "https://example.com" {
		t.Fatalf("openURL = %q", ctrl.openURL)
	}
	if ctrl.openGroupOpts != (browser.TabGroupOptions{GroupID: "9", Name: "workspace-2", Color: "cyan"}) {
		t.Fatalf("openGroupOpts = %+v", ctrl.openGroupOpts)
	}
}

func TestBrowserListTabGroupsReturnsGroups(t *testing.T) {
	ctrl := &recordingController{}
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "brw_list_tab_groups",
			"arguments": map[string]any{},
		},
	})
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if !ctrl.listTabGroupsCalled {
		t.Fatal("ListTabGroups was not called")
	}
	resp := parseLineResponse(t, output.Bytes())
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	var groups []browser.TabGroup
	if err := json.Unmarshal([]byte(text), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "9" || groups[0].Title != "workspace-2" || groups[0].TabCount != 2 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestBrowserGroupTabsForwardsGroupID(t *testing.T) {
	ctrl := &recordingController{}
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "brw_group_tabs",
			"arguments": map[string]any{
				"tab_ids":  []string{"41", "42"},
				"group_id": "9",
				"name":     "workspace-2",
				"color":    "cyan",
			},
		},
	})
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if len(ctrl.groupTabIDs) != 2 || ctrl.groupTabIDs[0] != "41" || ctrl.groupTabIDs[1] != "42" {
		t.Fatalf("groupTabIDs = %+v", ctrl.groupTabIDs)
	}
	if ctrl.groupTabsOpts != (browser.TabGroupOptions{GroupID: "9", Name: "workspace-2", Color: "cyan"}) {
		t.Fatalf("groupTabsOpts = %+v", ctrl.groupTabsOpts)
	}
}

func TestBrowserEmulateDeviceForwardsOptions(t *testing.T) {
	ctrl := &recordingController{}
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "brw_emulate_device",
			"arguments": map[string]any{
				"device":      "pixel_7",
				"orientation": "landscape",
			},
		},
	})
	var output bytes.Buffer

	if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	if ctrl.emulationOpts.Device != "pixel_7" || ctrl.emulationOpts.Orientation != "landscape" {
		t.Fatalf("emulation options = %+v", ctrl.emulationOpts)
	}
	resp := parseLineResponse(t, output.Bytes())
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), `"ok":true`) {
		t.Fatalf("unexpected emulation response: %#v", result)
	}
}

func TestBrowserSnapshotModeAllPreservesExplicitFullInspection(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_snapshot","arguments":{"mode":"all","include_hidden":true}}}` + "\n"
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
		if tool["name"] == "brw_snapshot" {
			snapshotTool = tool
			break
		}
	}
	if snapshotTool == nil {
		t.Fatal("brw_snapshot tool not found")
	}
	description := snapshotTool["description"].(string)
	if !strings.Contains(description, `mode:"all"`) || !strings.Contains(description, "include_hidden:true") {
		t.Fatalf("snapshot description does not document debug escalation: %q", description)
	}
	props := snapshotTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["include_hidden"]; !ok {
		t.Fatalf("include_hidden missing from snapshot schema: %#v", props)
	}
	assertSchemaEnumIncludes(t, props["mode"].(map[string]any), "frontier", "all", "form_lens")
	assertSchemaEnumIncludes(t, props["format"].(map[string]any), "json", "compact")
}

func TestBrowserEmulateDeviceSchemaExposesEnums(t *testing.T) {
	var emulationTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "brw_emulate_device" {
			emulationTool = tool
			break
		}
	}
	if emulationTool == nil {
		t.Fatal("brw_emulate_device tool not found")
	}
	props := emulationTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	assertSchemaEnumIncludes(t, props["device"].(map[string]any), "iphone_se", "pixel_7", "ipad_mini", "custom", "clear")
	assertSchemaEnumIncludes(t, props["orientation"].(map[string]any), "portrait", "landscape")
}

func TestPlanAndBatchActionSchemasExposeEnums(t *testing.T) {
	var planTool, batchTool map[string]any
	for _, tool := range tools() {
		switch tool["name"] {
		case "brw_plan":
			planTool = tool
		case "brw_batch":
			batchTool = tool
		}
	}
	if planTool == nil || batchTool == nil {
		t.Fatalf("missing plan/batch tools: plan=%v batch=%v", planTool != nil, batchTool != nil)
	}
	planProps := planTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	planStepProps := planProps["steps"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	assertSchemaEnumIncludes(t, planStepProps["action"].(map[string]any), "click", "snapshot", "read", "focus_tab")
	assertSchemaEnumIncludes(t, planStepProps["direction"].(map[string]any), "up", "down", "left", "right")

	batchProps := batchTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	batchStepProps := batchProps["steps"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	assertSchemaEnumIncludes(t, batchStepProps["action"].(map[string]any), "click", "assert_visible", "assert_text", "assert_hidden")
	assertSchemaEnumIncludes(t, batchStepProps["direction"].(map[string]any), "up", "down", "left", "right")
}

func TestBrowserSnapshotSchemaDocumentsVisualIslands(t *testing.T) {
	var snapshotTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "brw_snapshot" {
			snapshotTool = tool
			break
		}
	}
	if snapshotTool == nil {
		t.Fatal("brw_snapshot tool not found")
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
		if tool["name"] == "brw_screenshot" {
			shotTool = tool
			break
		}
	}
	if shotTool == nil {
		t.Fatal("brw_screenshot tool not found")
	}
	desc := shotTool["description"].(string)
	if !strings.Contains(desc, "annotate") || !strings.Contains(desc, "Set-of-Marks") {
		t.Fatalf("brw_screenshot description does not document annotate/Set-of-Marks: %q", desc)
	}
	props := shotTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["annotate"]; !ok {
		t.Fatalf("annotate missing from brw_screenshot schema: %#v", props)
	}
}

func TestBrowserScreenshotAnnotateReturnsLegend(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_screenshot","arguments":{"annotate":true}}}` + "\n"
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
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_screenshot","arguments":{}}}` + "\n"
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

func TestPlanSnapshotStepReturnsResultPayload(t *testing.T) {
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "brw_plan",
			"arguments": map[string]any{
				"steps": []map[string]any{{"action": "snapshot"}},
			},
		},
	})
	var output bytes.Buffer

	if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	resp := parseLineResponse(t, output.Bytes())
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"result"`) || !strings.Contains(text, `"snapshot"`) {
		t.Fatalf("plan step did not include result and snapshot payloads: %s", text)
	}
}

func TestBrowserFindDefaultsToBoundedResults(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_find","arguments":{"query":"checkout"}}}` + "\n"
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
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_cancel","arguments":{"token":"op-7"}}}` + "\n"
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
		if tool["name"] == "brw_cancel" {
			cancelTool = tool
			break
		}
	}
	if cancelTool == nil {
		t.Fatal("brw_cancel tool not registered")
	}
	props := cancelTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"token", "tab_id"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("brw_cancel schema missing %s: %#v", prop, props)
		}
	}
}

func TestBrowserNotifyDispatchesToController(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_notify","arguments":{"kind":"needs_input","title":"MFA required","message":"Enter your one-time code"}}}` + "\n"
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
		if tool["name"] == "brw_notify" {
			notifyTool = tool
			break
		}
	}
	if notifyTool == nil {
		t.Fatal("brw_notify tool not registered")
	}
	props := notifyTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"kind", "title", "message"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("brw_notify schema missing %s: %#v", prop, props)
		}
	}
}

func TestToolSchemasExposeTabScopedErgonomics(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range tools() {
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{
		"brw_find",
		"brw_fill",
		"brw_select",
		"brw_press",
		"brw_scroll",
		"brw_wait_for",
		"brw_evaluate",
		"brw_click_xy",
		"brw_console",
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
	clickText := byName["brw_click_text"]
	if clickText == nil {
		t.Fatal("brw_click_text tool not found")
	}
	props := clickText["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"text", "role", "exact", "tab_id"} {
		if _, ok := props[prop]; !ok {
			t.Fatalf("brw_click_text missing %s: %#v", prop, props)
		}
	}
}

func TestUploadFileSchemaExposesFileChooserParams(t *testing.T) {
	var uploadTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "brw_upload_file" {
			uploadTool = tool
			break
		}
	}
	if uploadTool == nil {
		t.Fatal("brw_upload_file tool not found")
	}
	props := uploadTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"click_ref", "click_text"} {
		schema, ok := props[prop].(map[string]any)
		if !ok {
			t.Fatalf("brw_upload_file schema missing %s: %#v", prop, props)
		}
		desc, _ := schema["description"].(string)
		if !strings.Contains(desc, "intercept") {
			t.Fatalf("brw_upload_file %s description should explain dialog interception, got %q", prop, desc)
		}
		if !strings.Contains(desc, "iframe") {
			t.Fatalf("brw_upload_file %s description should mention iframe support, got %q", prop, desc)
		}
	}
}

func TestUploadOptionsJSONRoundTripsFileChooserFields(t *testing.T) {
	// The MCP, HTTP, and httpclient transports all (de)serialize the file-chooser
	// trigger via these JSON tags, so the round-trip is the contract.
	in := snapshot.UploadOptions{
		Path:      "/tmp/cv.pdf",
		ClickRef:  "e17",
		ClickText: "Upload résumé",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"click_ref":"e17"`) {
		t.Fatalf("expected click_ref in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"click_text":"Upload résumé"`) {
		t.Fatalf("expected click_text in JSON, got %s", data)
	}
	var out snapshot.UploadOptions
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ClickRef != in.ClickRef || out.ClickText != in.ClickText {
		t.Fatalf("round-trip mismatch: got %+v want click_ref=%q click_text=%q", out, in.ClickRef, in.ClickText)
	}

	// Backward compat: omitting both keeps them empty (default in-DOM path).
	var bare snapshot.UploadOptions
	if err := json.Unmarshal([]byte(`{"path":"/tmp/cv.pdf"}`), &bare); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	if bare.ClickRef != "" || bare.ClickText != "" {
		t.Fatalf("expected empty chooser fields when omitted, got %+v", bare)
	}
}

func TestBrowserNavigateToolRegistration(t *testing.T) {
	var navTool map[string]any
	for _, tool := range tools() {
		if tool["name"] == "brw_navigate" {
			navTool = tool
			break
		}
	}
	if navTool == nil {
		t.Fatal("brw_navigate tool not found")
	}
	schema := navTool["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["direction"]; !ok {
		t.Fatalf("brw_navigate schema missing direction: %#v", props)
	}
	if _, ok := props["tab_id"]; !ok {
		t.Fatalf("brw_navigate schema missing tab_id: %#v", props)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "direction" {
		t.Fatalf("brw_navigate required = %#v, want [direction]", schema["required"])
	}
}

func TestBrowserNavigateDispatch(t *testing.T) {
	ctrl := &recordingController{}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_navigate","arguments":{"direction":"back"}}}` + "\n"
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
		{"focus_tab accepts tab_id", "brw_focus_tab", `{"tab_id":"42"}`, "42", true},
		{"focus_tab accepts legacy id", "brw_focus_tab", `{"id":"7"}`, "7", true},
		{"focus_tab prefers tab_id over id", "brw_focus_tab", `{"id":"7","tab_id":"42"}`, "42", true},
		{"close_tab accepts tab_id", "brw_close_tab", `{"tab_id":"99"}`, "99", false},
		{"close_tab accepts legacy id", "brw_close_tab", `{"id":"3"}`, "3", false},
		{"close_tab prefers tab_id over id", "brw_close_tab", `{"id":"3","tab_id":"99"}`, "99", false},
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

func TestCloseContextAcceptsLegacyBrowserContextID(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"accepts context_id", `{"context_id":"ctx-new"}`, "ctx-new"},
		{"accepts legacy browser_context_id", `{"browser_context_id":"ctx-legacy"}`, "ctx-legacy"},
		{"prefers context_id", `{"context_id":"ctx-new","browser_context_id":"ctx-legacy"}`, "ctx-new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &recordingController{}
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_close_context","arguments":` + tc.args + `}}` + "\n"
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			if ctrl.closeContextID != tc.want {
				t.Fatalf("closeContextID = %q, want %q", ctrl.closeContextID, tc.want)
			}
		})
	}
}

func TestFocusAndCloseTabSchemasExposeTabIDAlias(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range tools() {
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{"brw_focus_tab", "brw_close_tab"} {
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
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_click_text","arguments":` + tc.args + `}}` + "\n"
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
		if tool["name"] == "brw_evaluate" {
			evalTool = tool
			break
		}
	}
	if evalTool == nil {
		t.Fatal("brw_evaluate tool not registered")
	}
	desc := evalTool["description"].(string)
	if !strings.Contains(strings.ToLower(desc), "content-security-policy") && !strings.Contains(strings.ToUpper(desc), "CSP") {
		t.Fatalf("brw_evaluate description does not mention CSP: %q", desc)
	}
	if !strings.Contains(strings.ToLower(desc), "cross-origin") {
		t.Fatalf("brw_evaluate description does not mention cross-origin caveat: %q", desc)
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
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_click","arguments":` + tc.args + `}}` + "\n"
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
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_drag","arguments":{"from":{"ref":"e1"},"to":{"x":200,"y":50},"steps":6,"button":"left"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brw_mouse_down","arguments":{"ref":"e1","button":"left"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brw_mouse_up","arguments":{"x":12,"y":34}}}`,
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
	for _, name := range []string{"brw_drag", "brw_mouse_down", "brw_mouse_up"} {
		if byName[name] == nil {
			t.Fatalf("%s tool not registered", name)
		}
	}
	clickProps := byName["brw_click"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"button", "click_count", "x", "y"} {
		if _, ok := clickProps[prop]; !ok {
			t.Fatalf("brw_click schema missing %s: %#v", prop, clickProps)
		}
	}
	dragProps := byName["brw_drag"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, prop := range []string{"from", "to", "steps", "button"} {
		if _, ok := dragProps[prop]; !ok {
			t.Fatalf("brw_drag schema missing %s: %#v", prop, dragProps)
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
	snapshotOpts        snapshot.SnapshotOptions
	findOpts            snapshot.FindOptions
	navigateDirection   string
	cancelToken         string
	notifyOpts          browser.NotifyOptions
	clickRef            string
	clickButton         browser.ClickButtonOptions
	dragOpts            browser.DragOptions
	mouseDownOpt        browser.MouseButtonOptions
	mouseUpOpt          browser.MouseButtonOptions
	focusID             string
	closeID             string
	closeContextID      string
	clickTextOpts       snapshot.ClickTextOptions
	openURL             string
	openGroupOpts       browser.TabGroupOptions
	listTabGroupsCalled bool
	groupTabIDs         []string
	groupTabsOpts       browser.TabGroupOptions
	emulationOpts       browser.DeviceEmulationOptions
}

func (r *recordingController) OpenInGroup(ctx context.Context, targetURL string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	r.openURL = targetURL
	r.openGroupOpts = opts
	return browser.OpenResult{Tab: browser.Tab{ID: "tab1", URL: targetURL, GroupID: opts.GroupID, GroupTitle: opts.Name, GroupColor: opts.Color}}, nil
}

func (r *recordingController) ListTabGroups(context.Context) ([]browser.TabGroup, error) {
	r.listTabGroupsCalled = true
	return []browser.TabGroup{{
		ID:       "9",
		Title:    "workspace-2",
		Color:    "cyan",
		WindowID: 7,
		TabIDs:   []string{"41", "42"},
		TabCount: 2,
	}}, nil
}

func (r *recordingController) GroupTabs(ctx context.Context, tabIDs []string, opts browser.TabGroupOptions) error {
	r.groupTabIDs = append([]string(nil), tabIDs...)
	r.groupTabsOpts = opts
	return nil
}

func (r *recordingController) FocusTab(ctx context.Context, id string) error {
	r.focusID = id
	return nil
}

func (r *recordingController) CloseTab(ctx context.Context, id string) error {
	r.closeID = id
	return nil
}

func (r *recordingController) CloseContext(ctx context.Context, contextID string) error {
	r.closeContextID = contextID
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

func (r *recordingController) EmulateDevice(ctx context.Context, opts browser.DeviceEmulationOptions) (browser.DeviceEmulationResult, error) {
	r.emulationOpts = opts
	return browser.DeviceEmulationResult{OK: true, Emulation: &browser.DeviceEmulationConfig{Device: "iphone_se", Width: 375, Height: 667, DeviceScaleFactor: 2, Mobile: true, Touch: true, Orientation: "portrait"}}, nil
}

func (fakeController) Open(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}
func (fakeController) OpenIncognito(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "t1", BrowserContextID: "ctx-1"}}, nil
}
func (fakeController) CloseContext(context.Context, string) error      { return nil }
func (fakeController) ListTabs(context.Context) ([]browser.Tab, error) { return nil, nil }
func (fakeController) ListTabGroups(context.Context) ([]browser.TabGroup, error) {
	return nil, nil
}
func (fakeController) FocusTab(context.Context, string) error { return nil }
func (fakeController) CloseTab(context.Context, string) error { return nil }
func (fakeController) EmulateDevice(context.Context, browser.DeviceEmulationOptions) (browser.DeviceEmulationResult, error) {
	return browser.DeviceEmulationResult{OK: true}, nil
}
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
func (fakeController) ScreenshotAnnotated(context.Context, browser.AnnotatedScreenshotOptions) (browser.AnnotatedScreenshot, error) {
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
	snap := snapshot.PageSnapshot{URL: "https://example.com", Title: "Example"}
	return browser.PlanResult{
		OK: true,
		Steps: []browser.PlanStepResult{{
			Index:    0,
			Action:   "snapshot",
			OK:       true,
			Message:  "snapshot captured",
			Result:   snap,
			Snapshot: &snap,
		}},
		StepsCompleted: 1,
	}, nil
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
func (fakeController) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}
func (fakeController) GroupTabs(context.Context, []string, browser.TabGroupOptions) error {
	return nil
}
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

func TestServe_MalformedJSON(t *testing.T) {
	input := "not valid json\n"
	var output bytes.Buffer
	err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseLineResponse(t, output.Bytes())
	if resp["error"] == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestServe_UnknownMethod(t *testing.T) {
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "bogus/method",
	})
	var output bytes.Buffer
	err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseLineResponse(t, output.Bytes())
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp["error"])
	}
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("expected method-not-found code -32601, got %v", errObj["code"])
	}
}

func TestServe_UnknownTool(t *testing.T) {
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "bogus_tool",
			"arguments": map[string]any{},
		},
	})
	var output bytes.Buffer
	err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseLineResponse(t, output.Bytes())
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp["error"])
	}
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected invalid-params code -32602, got %v", errObj["code"])
	}
}

// errorController returns errors for specific methods.
type errorController struct {
	fakeController
}

func (errorController) Open(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, fmt.Errorf("chrome crashed")
}

func TestServe_ControllerError(t *testing.T) {
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "brw_open",
			"arguments": map[string]any{"url": "https://example.com"},
		},
	})
	var output bytes.Buffer
	err := New(errorController{}).Serve(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseLineResponse(t, output.Bytes())
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected content array, got %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content item, got %v", content)
	}
	if first["text"] != "chrome crashed" {
		t.Fatalf("expected error text 'chrome crashed', got %v", first["text"])
	}
}

func assertSchemaEnumIncludes(t *testing.T, schema map[string]any, values ...string) {
	t.Helper()
	raw, ok := schema["enum"].([]string)
	if !ok {
		items, ok := schema["enum"].([]any)
		if !ok {
			t.Fatalf("schema has no enum: %#v", schema)
		}
		raw = make([]string, 0, len(items))
		for _, item := range items {
			raw = append(raw, item.(string))
		}
	}
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		seen[item] = true
	}
	for _, value := range values {
		if !seen[value] {
			t.Fatalf("enum %v does not include %q", raw, value)
		}
	}
}

func lineJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func parseLineResponse(t *testing.T, data []byte) map[string]any {
	t.Helper()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		t.Fatal("empty response")
	}
	var resp map[string]any
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	return resp
}
