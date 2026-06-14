package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type Controller interface {
	Open(context.Context, string) (browser.OpenResult, error)
	OpenInGroup(context.Context, string, string) (browser.OpenResult, error)
	ListTabs(context.Context) ([]browser.Tab, error)
	FocusTab(context.Context, string) error
	CloseTab(context.Context, string) error
	Read(context.Context) (readability.PageRead, error)
	Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Click(context.Context, string) (browser.ActionResult, error)
	Hover(context.Context, string) (browser.ActionResult, error)
	Type(context.Context, string, string) (browser.ActionResult, error)
	Fill(context.Context, snapshot.FillOptions) (browser.ActionResult, error)
	UploadFile(context.Context, snapshot.UploadOptions) (browser.ActionResult, error)
	Select(context.Context, string, string) (browser.ActionResult, error)
	Press(context.Context, string) (browser.ActionResult, error)
	Scroll(context.Context, string) (browser.ActionResult, error)
	Screenshot(context.Context) (browser.Screenshot, error)
	ScreenshotElement(context.Context, string) (browser.Screenshot, error)
	WaitFor(context.Context, string, time.Duration) error
	Evaluate(context.Context, string) (any, error)
	NetworkRequests(context.Context, string) ([]browser.NetworkRequest, error)
	ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error)
}

type Server struct {
	manager Controller
}

const (
	defaultSnapshotMode  = "frontier"
	defaultSnapshotLimit = 40
	defaultFindLimit     = 20
)

func New(manager Controller) *Server {
	return &Server{manager: manager}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	mode := stdioModeUnknown
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		body, nextMode, err := readMessage(reader, mode)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if nextMode != stdioModeUnknown {
			mode = nextMode
		}
		if len(bytes.TrimSpace(body)) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			if err := writeMessage(out, mode, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		result, rpcErr := s.handle(ctx, req.Method, req.Params)
		resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}
		if err := writeMessage(out, mode, resp); err != nil {
			return err
		}
	}
}

type stdioMode int

const (
	stdioModeUnknown stdioMode = iota
	stdioModeFramed
	stdioModeLine
)

func readMessage(r *bufio.Reader, mode stdioMode) ([]byte, stdioMode, error) {
	if mode == stdioModeLine {
		line, err := readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		return bytes.TrimSpace(line), mode, nil
	}

	line, err := readLineAllowEOF(r)
	if err != nil {
		return nil, mode, err
	}
	trimmed := strings.TrimSpace(string(line))
	for trimmed == "" {
		line, err = readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		trimmed = strings.TrimSpace(string(line))
	}

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || !strings.Contains(trimmed, ":") {
		return []byte(trimmed), stdioModeLine, nil
	}

	headers := map[string]string{}
	for {
		if trimmed == "" {
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, mode, fmt.Errorf("invalid MCP stdio header %q", trimmed)
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		line, err = readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		trimmed = strings.TrimSpace(string(line))
	}

	rawLen, ok := headers["content-length"]
	if !ok {
		return nil, mode, errors.New("missing Content-Length header")
	}
	length, err := strconv.Atoi(rawLen)
	if err != nil || length < 0 {
		return nil, mode, fmt.Errorf("invalid Content-Length %q", rawLen)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, mode, err
	}
	return body, stdioModeFramed, nil
}

func readLineAllowEOF(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err == nil {
		return line, nil
	}
	if err == io.EOF && len(line) > 0 {
		return line, nil
	}
	return nil, err
}

func writeMessage(w io.Writer, mode stdioMode, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if mode == stdioModeFramed {
		if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo": map[string]any{
				"name":    "agent-browserd",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
		}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, invalid(err)
		}
		return s.callTool(ctx, call.Name, call.Arguments)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	switch name {
	case "browser_open":
		var req struct {
			URL   string `json:"url"`
			Group string `json:"group"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Group != "" {
			return toolJSON(s.manager.OpenInGroup(ctx, req.URL, req.Group))
		}
		return toolJSON(s.manager.Open(ctx, req.URL))
	case "browser_list_tabs":
		return toolJSON(s.manager.ListTabs(ctx))
	case "browser_focus_tab":
		var req struct {
			ID string `json:"id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.FocusTab(ctx, req.ID))
	case "browser_close_tab":
		var req struct {
			ID string `json:"id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CloseTab(ctx, req.ID))
	case "browser_read":
		return toolJSON(s.manager.Read(ctx))
	case "browser_snapshot":
		var req snapshot.SnapshotOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPSnapshotOptions(req)
		return toolJSON(s.manager.Snapshot(ctx, req))
	case "browser_find":
		var req snapshot.FindOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPFindOptions(req)
		return toolJSON(s.manager.Find(ctx, req))
	case "browser_click":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Click(ctx, req.Ref))
	case "browser_hover":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Hover(ctx, req.Ref))
	case "browser_type":
		var req struct {
			Ref  string `json:"ref"`
			Text string `json:"text"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Type(ctx, req.Ref, req.Text))
	case "browser_fill":
		req := snapshot.FillOptions{Replace: true}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Fill(ctx, req))
	case "browser_upload_file":
		var req snapshot.UploadOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.UploadFile(ctx, req))
	case "browser_select":
		var req struct {
			Ref   string `json:"ref"`
			Value string `json:"value"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Select(ctx, req.Ref, req.Value))
	case "browser_press":
		var req struct {
			Key string `json:"key"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Press(ctx, req.Key))
	case "browser_scroll":
		var req struct {
			Direction string `json:"direction"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Scroll(ctx, req.Direction))
	case "browser_screenshot":
		shot, err := s.manager.Screenshot(ctx)
		if err != nil {
			return toolError(err), nil
		}
		return map[string]any{
			"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
		}, nil
	case "browser_screenshot_element":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		shot, err := s.manager.ScreenshotElement(ctx, req.Ref)
		if err != nil {
			return toolError(err), nil
		}
		return map[string]any{
			"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
		}, nil
	case "browser_wait_for":
		var req struct {
			Condition string `json:"condition"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.WaitFor(ctx, req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "browser_evaluate":
		var req struct {
			Expression string `json:"expression"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Evaluate(ctx, req.Expression))
	case "browser_network_requests":
		var req struct {
			Filter string `json:"filter"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.NetworkRequests(ctx, req.Filter))
	case "browser_plan":
		var req struct {
			Steps []browser.PlanStep `json:"steps"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ExecutePlan(ctx, req.Steps))
	default:
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", name)}
	}
}

func normalizeMCPSnapshotOptions(opts snapshot.SnapshotOptions) snapshot.SnapshotOptions {
	opts.Mode = strings.TrimSpace(strings.ToLower(opts.Mode))
	if opts.Mode == "" {
		opts.Mode = defaultSnapshotMode
	}
	if opts.Mode == defaultSnapshotMode {
		opts.ViewportOnly = true
		if opts.Limit <= 0 {
			opts.Limit = defaultSnapshotLimit
		}
	} else if opts.Limit < 0 {
		opts.Limit = 0
	}
	return opts
}

func normalizeMCPFindOptions(opts snapshot.FindOptions) snapshot.FindOptions {
	if opts.Limit <= 0 {
		opts.Limit = defaultFindLimit
	}
	return opts
}

func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	return json.Unmarshal(args, dst)
}

func toolJSON[T any](value T, err error) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{
		"content":           []toolContent{{Type: "text", Text: string(data)}},
		"structuredContent": value,
	}, nil
}

func toolOK(err error) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{"content": []toolContent{{Type: "text", Text: `{"ok":true}`}}}, nil
}

func toolError(err error) any {
	return map[string]any{
		"isError": true,
		"content": []toolContent{{Type: "text", Text: err.Error()}},
	}
}

func invalid(err error) *rpcError {
	return &rpcError{Code: -32602, Message: err.Error()}
}

func tools() []map[string]any {
	return []map[string]any{
		tool("browser_open", "Open a URL in a visible Chrome/Chromium tab.", object(map[string]any{
			"url":   stringSchema("URL to open. Scheme defaults to https."),
			"group": stringSchema("Optional Chrome tab group name. When set, the new tab is added to a tab group with this title."),
		}, []string{"url"})),
		tool("browser_list_tabs", "List controllable Chrome/Chromium browser targets, including tabs and popup windows when the extension bridge reports them.", object(nil, nil)),
		tool("browser_focus_tab", "Focus a controllable Chrome/Chromium target by id and make it the default target for following reads/actions.", object(map[string]any{
			"id": stringSchema("Target id from browser_list_tabs."),
		}, []string{"id"})),
		tool("browser_close_tab", "Close a controllable Chrome/Chromium target by id.", object(map[string]any{
			"id": stringSchema("Target id from browser_list_tabs."),
		}, []string{"id"})),
		tool("browser_read", "Return semantic page content: main text, headings, links, forms, tables, and metadata.", object(nil, nil)),
		tool("browser_snapshot", "Return interactive controls with stable refs. Defaults to a bounded visible/actionable viewport frontier; use mode:\"all\" for full-page debugging (returns every matching element including offscreen/hidden controls — useful for comprehensive page analysis), and add include_hidden:true only when hidden inputs are needed. Metadata includes total_candidates for the full count before filtering.", object(map[string]any{
			"mode":           stringSchema("frontier (default, scored visible/actionable controls) or all (full matching list, including offscreen/currently invisible matching controls)."),
			"query":          stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value."),
			"text":           stringSchema("Alias for query-style text filtering."),
			"role":           stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":          integerSchema("Maximum number of elements to return. Defaults to 40 in frontier mode."),
			"viewport_only":  boolSchema("Only return elements intersecting the viewport. Forced true in default frontier mode."),
			"include_hidden": boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
			"include_ax":     boolSchema("Include full accessibility-tree enrichment. Expensive; defaults false."),
			"since":          integerSchema("Reserved page-state version for future delta snapshots."),
		}, nil)),
		tool("browser_find", "Find matching semantic element refs without dumping the full page.", object(map[string]any{
			"query":          stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value."),
			"text":           stringSchema("Alias for query-style text filtering."),
			"role":           stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":          integerSchema("Maximum number of elements to return."),
			"viewport_only":  boolSchema("Only return elements intersecting the viewport."),
			"include_hidden": boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
		}, nil)),
		tool("browser_click", "Click a semantic element ref from browser_snapshot.", object(map[string]any{
			"ref": stringSchema("Element ref, for example e18."),
		}, []string{"ref"})),
		tool("browser_hover", "Hover over a semantic element ref to trigger mouseenter/mouseover/pointermove events.", object(map[string]any{
			"ref": stringSchema("Element ref, for example e18."),
		}, []string{"ref"})),
		tool("browser_evaluate", "Run arbitrary JavaScript in the page context and return the JSON-serializable result. Supports async expressions.", object(map[string]any{
			"expression": stringSchema("JavaScript expression to evaluate. May use await for async operations."),
		}, []string{"expression"})),
		tool("browser_network_requests", "Return network resource requests captured by the Performance API (performance.getEntriesByType).", object(map[string]any{
			"filter": stringSchema("Optional case-insensitive substring to filter request URLs."),
		}, nil)),
		tool("browser_type", "Type text into a semantic element ref.", object(map[string]any{
			"ref":  stringSchema("Element ref, for example e17."),
			"text": stringSchema("Text to insert."),
		}, []string{"ref", "text"})),
		tool("browser_fill", "Replace or append text in a semantic text field by ref or query and return a post-action observation.", object(map[string]any{
			"ref":     stringSchema("Element ref, for example e17. Optional when query is supplied."),
			"query":   stringSchema("Find a fillable target by semantic name when ref is not supplied."),
			"role":    stringSchema("Optional role filter when using query, normally textbox or searchbox."),
			"text":    stringSchema("Text to put in the field."),
			"replace": boolSchema("Replace existing field content instead of appending. Defaults to true."),
		}, []string{"text"})),
		tool("browser_upload_file", "Set one or more local files on a semantic file input by ref or query and return a post-action observation.", object(map[string]any{
			"ref":   stringSchema("Element ref for input[type=file]. Optional when query is supplied."),
			"query": stringSchema("Find a file input by semantic name when ref is not supplied. Defaults to file."),
			"role":  stringSchema("Optional role filter when using query."),
			"path":  stringSchema("Single local file path on the browser host."),
			"paths": map[string]any{"type": "array", "items": stringSchema("Local file path on the browser host."), "description": "One or more local file paths on the browser host."},
		}, nil)),
		tool("browser_select", "Set a select/listbox value by semantic element ref.", object(map[string]any{
			"ref":   stringSchema("Element ref for a select element."),
			"value": stringSchema("Option value to select."),
		}, []string{"ref", "value"})),
		tool("browser_press", "Press a keyboard key in the active tab.", object(map[string]any{
			"key": stringSchema("Key name or chord, for example Enter, Tab, Escape, ArrowDown, Meta+Enter."),
		}, []string{"key"})),
		tool("browser_scroll", "Scroll the active page or scroll container in a direction.", object(map[string]any{
			"direction": stringSchema("up, down, left, or right."),
		}, []string{"direction"})),
		tool("browser_screenshot", "Capture a PNG screenshot for visual fallback/debugging.", object(nil, nil)),
		tool("browser_screenshot_element", "Capture a PNG screenshot of a semantic element ref for visual fallback/debugging.", object(map[string]any{
			"ref": stringSchema("Element ref from browser_snapshot."),
		}, []string{"ref"})),
		tool("browser_wait_for", "Wait for page readiness, URL/title/text substring, or ref availability.", object(map[string]any{
			"condition":  stringSchema("load, text:..., not_text:..., url:..., not_url:..., title:..., ref:..., or plain text."),
			"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in milliseconds."},
		}, []string{"condition"})),
		tool("browser_plan", "Execute a sequence of browser operations in one round-trip. Steps run sequentially and stop on first failure.", object(map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered list of steps to execute.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":      stringSchema("One of: click, type, fill, select, press, scroll, hover, wait, snapshot, open, focus_tab."),
						"ref":         stringSchema("Element ref for click, type, fill, select, hover."),
						"text":        stringSchema("Text for type and fill actions."),
						"value":       stringSchema("Option value for select action."),
						"direction":   stringSchema("Scroll direction: up, down, left, right."),
						"condition":   stringSchema("Wait condition (load, text:..., ref:..., url:..., etc)."),
						"timeout_ms":  map[string]any{"type": "integer", "description": "Timeout for wait action in milliseconds."},
						"url":         stringSchema("URL for open action."),
						"id":          stringSchema("Tab id for focus_tab action."),
						"key":         stringSchema("Key name for press action (Enter, Tab, Escape, etc)."),
						"expect_ref":  stringSchema("Validate this ref exists before running the action (fail-fast)."),
						"expect_role": stringSchema("Validate the expect_ref element has this role."),
					},
					"required": []string{"action"},
				},
			},
		}, []string{"steps"})),
	}
}

func tool(name, description string, schema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": schema}
}

func object(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func integerSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
