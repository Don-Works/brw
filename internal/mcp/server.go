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

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

type Server struct {
	manager     browser.Controller
	toolProfile string // "all" (default) or "core"
}

const (
	defaultSnapshotMode  = "frontier"
	defaultSnapshotLimit = 40
	defaultFindLimit     = 20
)

// coreToolNames is the lean, common-flow tool surface. It hides the long tail
// behind the default "all" profile while keeping the verbs an agent needs for
// common read/click/type/select/navigate/scroll/drag/upload/hover flows.
var coreToolNames = map[string]bool{
	"brw_open":        true,
	"brw_list_tabs":   true,
	"brw_focus_tab":   true,
	"brw_read":        true,
	"brw_snapshot":    true,
	"brw_find":        true,
	"brw_click":       true,
	"brw_click_text":  true,
	"brw_type":        true,
	"brw_fill":        true,
	"brw_select":      true,
	"brw_press":       true,
	"brw_scroll":      true,
	"brw_hover":       true,
	"brw_drag":        true,
	"brw_upload_file": true,
	"brw_navigate":    true,
	"brw_wait_for":    true,
	"brw_batch":       true,
	"brw_screenshot":  true,
}

func New(manager browser.Controller) *Server {
	return &Server{manager: manager, toolProfile: "all"}
}

// NewWithToolProfile builds a server exposing only the named tool profile in
// tools/list ("core" for the lean surface, anything else for the full surface).
// All tools remain callable regardless of profile; the profile only narrows what
// tools/list advertises.
func NewWithToolProfile(manager browser.Controller, profile string) *Server {
	if profile == "" {
		profile = "all"
	}
	return &Server{manager: manager, toolProfile: profile}
}

// advertisedTools returns the tool list for tools/list, narrowed to the active
// profile. "core" filters to coreToolNames; any other value returns everything.
func (s *Server) advertisedTools() []map[string]any {
	all := tools()
	if s.toolProfile != "core" {
		return all
	}
	filtered := make([]map[string]any, 0, len(coreToolNames))
	for _, t := range all {
		if name, _ := t["name"].(string); coreToolNames[name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
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
				"name":    "brw",
				"version": "0.0.1",
			},
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
		}, nil
	case "tools/list":
		return map[string]any{"tools": s.advertisedTools()}, nil
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

// activeTabResolver is the optional capability a Controller may implement to
// resolve the genuinely focused tab once per top-level tool call. Only the
// extension Bridge implements it (its per-call active-tab resolution is the
// multiplier we are collapsing); the direct-CDP Manager and the HTTP proxy do
// not, so they are left entirely unchanged.
type activeTabResolver interface {
	ResolveActiveTabID(context.Context) string
}

// tabAgnosticTools lists tools that must NOT trigger a one-shot active-tab
// resolution: tab-management verbs (which manage focus themselves) and the
// batch/plan runners (which resolve once internally and re-pin per step after a
// focus_tab/open). list_tabs in particular must stay free of the extra round
// trip the task brief calls out.
var tabAgnosticTools = map[string]bool{
	"brw_list_tabs":       true,
	"brw_list_tab_groups": true,
	"brw_focus_tab":       true,
	"brw_close_tab":       true,
	"brw_open":            true,
	"brw_open_incognito":  true,
	"brw_close_context":   true,
	"brw_group_tabs":      true,
	"brw_ungroup_tabs":    true,
	"brw_batch":           true,
	"brw_plan":            true,
	"brw_cancel":          true,
	"brw_trace":           true,
	"brw_clear_trace":     true,
}

// pinActiveTabForTool resolves the active tab once (when the controller supports
// it and the tool acts on the active tab) and pins it into the context via
// browser.WithTabID. A no-op when the controller does not implement
// activeTabResolver, the tool is tab-management/batch, or resolution fails.
func pinActiveTabForTool(ctx context.Context, manager browser.Controller, name string) context.Context {
	if tabAgnosticTools[name] {
		return ctx
	}
	resolver, ok := manager.(activeTabResolver)
	if !ok {
		return ctx
	}
	if tabID := resolver.ResolveActiveTabID(ctx); tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	name = canonicalToolName(name)

	// Extract optional tab_id from any tool call and inject into context
	var tabProbe struct {
		TabID string `json:"tab_id"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &tabProbe)
	}
	if tabProbe.TabID != "" {
		ctx = browser.WithTabID(ctx, tabProbe.TabID)
	} else {
		// No explicit tab_id: for tools that act on the active tab, resolve it
		// ONCE here and pin it into the context so every downstream page call
		// short-circuits instead of re-resolving the active tab per sub-call
		// (the extension bridge otherwise issues get_active_tab_id 3-11x per
		// logical tool call). Tab-management tools and the batch/plan runners are
		// excluded: they manage focus themselves or pin internally per step.
		ctx = pinActiveTabForTool(ctx, s.manager, name)
	}
	switch name {
	case "brw_open":
		var req struct {
			URL        string `json:"url"`
			Group      string `json:"group"`
			GroupID    string `json:"group_id"`
			GroupColor string `json:"group_color"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Group != "" || req.GroupID != "" {
			return toolJSON(s.manager.OpenInGroup(ctx, req.URL, browser.TabGroupOptions{
				GroupID: req.GroupID,
				Name:    req.Group,
				Color:   req.GroupColor,
			}))
		}
		return toolJSON(s.manager.Open(ctx, req.URL))
	case "brw_open_incognito":
		var req struct {
			URL string `json:"url"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.OpenIncognito(ctx, req.URL))
	case "brw_close_context":
		var req struct {
			BrowserContextID       string `json:"context_id"`
			LegacyBrowserContextID string `json:"browser_context_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CloseContext(ctx, contextIDArg(req.BrowserContextID, req.LegacyBrowserContextID)))
	case "brw_list_tabs":
		return toolJSON(s.manager.ListTabs(ctx))
	case "brw_list_tab_groups":
		return toolJSON(s.manager.ListTabGroups(ctx))
	case "brw_focus_tab":
		var req struct {
			ID    string `json:"id"`
			TabID string `json:"tab_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.FocusTab(ctx, tabIDArg(req.TabID, req.ID)))
	case "brw_close_tab":
		var req struct {
			ID    string `json:"id"`
			TabID string `json:"tab_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CloseTab(ctx, tabIDArg(req.TabID, req.ID)))
	case "brw_read":
		return toolJSON(s.manager.Read(ctx))
	case "brw_read_data":
		return toolJSON(s.manager.ReadData(ctx))
	case "brw_snapshot":
		var req snapshot.SnapshotOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPSnapshotOptions(req)
		return toolJSON(s.manager.Snapshot(ctx, req))
	case "brw_find":
		var req snapshot.FindOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPFindOptions(req)
		return toolJSON(s.manager.Find(ctx, req))
	case "brw_click":
		var req struct {
			Ref        string   `json:"ref"`
			X          *float64 `json:"x"`
			Y          *float64 `json:"y"`
			Button     string   `json:"button"`
			ClickCount int      `json:"click_count"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// Plain left single-click on a ref keeps the fast in-page click path.
		// Any non-default button/count, or a coordinate target, routes through
		// the decomposed CDP click so right/double/triple/middle clicks and
		// canvas coordinate clicks all share one tool.
		if browser.IsDefaultLeftSingleRefClick(req.Button, req.ClickCount, req.Ref, req.X, req.Y) {
			return toolJSON(s.manager.Click(ctx, req.Ref))
		}
		return toolJSON(s.manager.ClickButton(ctx, browser.ClickButtonOptions{
			MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
			Button:     req.Button,
			ClickCount: req.ClickCount,
		}))
	case "brw_drag":
		var req struct {
			From   browser.MousePoint `json:"from"`
			To     browser.MousePoint `json:"to"`
			Steps  int                `json:"steps"`
			Button string             `json:"button"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Drag(ctx, browser.DragOptions{
			From:   req.From,
			To:     req.To,
			Steps:  req.Steps,
			Button: req.Button,
		}))
	case "brw_mouse_down":
		opts, err := parseMouseButtonArgs(args)
		if err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.MouseDown(ctx, opts))
	case "brw_mouse_up":
		opts, err := parseMouseButtonArgs(args)
		if err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.MouseUp(ctx, opts))
	case "brw_click_text":
		var req snapshot.ClickTextOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ClickText(ctx, req))
	case "brw_navigate":
		var req struct {
			Direction string `json:"direction"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Navigate(ctx, req.Direction))
	case "brw_hover":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Hover(ctx, req.Ref))
	case "brw_type":
		var req struct {
			Ref  string `json:"ref"`
			Text string `json:"text"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Type(ctx, req.Ref, req.Text))
	case "brw_fill":
		req := snapshot.FillOptions{Replace: true}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Fill(ctx, req))
	case "brw_upload_file":
		var req snapshot.UploadOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.UploadFile(ctx, req))
	case "brw_select":
		var req struct {
			Ref   string `json:"ref"`
			Value string `json:"value"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Select(ctx, req.Ref, req.Value))
	case "brw_press":
		var req struct {
			Key string `json:"key"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Press(ctx, req.Key))
	case "brw_scroll":
		var req struct {
			Direction string `json:"direction"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Scroll(ctx, req.Direction))
	case "brw_screenshot":
		var req struct {
			Annotate bool   `json:"annotate"`
			Ref      string `json:"ref"`
			Region   *struct {
				X      float64 `json:"x"`
				Y      float64 `json:"y"`
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"region"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// A ref or region implies an annotated (Set-of-Marks) crop even if annotate
		// was omitted — the whole point of the crop is the ref legend.
		if req.Annotate || strings.TrimSpace(req.Ref) != "" || req.Region != nil {
			aopts := browser.AnnotatedScreenshotOptions{Mode: "frontier", Ref: req.Ref}
			if req.Region != nil {
				aopts.Region = browser.ScreenshotRegion{X: req.Region.X, Y: req.Region.Y, Width: req.Region.Width, Height: req.Region.Height}
			}
			shot, err := s.manager.ScreenshotAnnotated(ctx, aopts)
			if err != nil {
				return toolError(err), nil
			}
			return map[string]any{
				"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
				"legend":  shot.Legend,
			}, nil
		}
		shot, err := s.manager.Screenshot(ctx)
		if err != nil {
			return toolError(err), nil
		}
		return map[string]any{
			"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
		}, nil
	case "brw_screenshot_element":
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
	case "brw_wait_for":
		var req struct {
			Condition string `json:"condition"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.WaitFor(ctx, req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_evaluate":
		var req struct {
			Expression string `json:"expression"`
			Offset     int    `json:"offset"`
			MaxBytes   int    `json:"max_bytes"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		value, err := s.manager.Evaluate(ctx, req.Expression)
		return evaluateResult(value, err, req.Offset, req.MaxBytes)
	case "brw_network_requests":
		var req struct {
			Filter string `json:"filter"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.NetworkRequests(ctx, req.Filter))
	case "brw_network_capture":
		var req struct {
			Filter string `json:"filter"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.NetworkCapture(ctx, req.Filter))
	case "brw_replay_request":
		var req struct {
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ReplayRequest(ctx, browser.ReplayRequestParams{
			Method:  req.Method,
			URL:     req.URL,
			Headers: req.Headers,
			Body:    req.Body,
		}))
	case "brw_plan":
		var req struct {
			Steps []browser.PlanStep `json:"steps"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ExecutePlan(ctx, req.Steps))
	case "brw_batch":
		var req struct {
			Steps []browser.BatchStep `json:"steps"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ExecuteBatch(ctx, req.Steps))
	case "brw_cancel":
		var req struct {
			Token string `json:"token"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Cancel(ctx, req.Token))
	case "brw_observe":
		return toolJSON(s.manager.Observe(ctx))
	case "brw_group_tabs":
		var req struct {
			TabIDs  []string `json:"tab_ids"`
			Name    string   `json:"name"`
			Color   string   `json:"color"`
			GroupID string   `json:"group_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.GroupTabs(ctx, req.TabIDs, browser.TabGroupOptions{
			GroupID: req.GroupID,
			Name:    req.Name,
			Color:   req.Color,
		}))
	case "brw_ungroup_tabs":
		var req struct {
			TabIDs []string `json:"tab_ids"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.UngroupTabs(ctx, req.TabIDs))
	case "brw_assert_visible":
		var req struct {
			Ref       string `json:"ref"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertVisible(ctx, req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_text":
		var req struct {
			Ref       string `json:"ref"`
			Text      string `json:"text"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertText(ctx, req.Ref, req.Text, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_value":
		var req struct {
			Ref       string `json:"ref"`
			Value     string `json:"value"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertValue(ctx, req.Ref, req.Value, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_hidden":
		var req struct {
			Ref       string `json:"ref"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertHidden(ctx, req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_commit":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CommitField(ctx, req.Ref))
	case "brw_notify":
		var req struct {
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Notify(ctx, browser.NotifyOptions{Kind: req.Kind, Title: req.Title, Message: req.Message}))
	case "brw_click_xy":
		var req struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ClickXY(ctx, req.X, req.Y))
	case "brw_console":
		return toolJSON(s.manager.ConsoleMessages(ctx))
	case "brw_downloads":
		return toolJSON(s.manager.Downloads(ctx))
	case "brw_trace":
		trace := s.manager.GetTrace()
		return toolJSON(trace, nil)
	case "brw_clear_trace":
		s.manager.ClearTrace()
		return toolOK(nil)
	default:
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", name)}
	}
}

func normalizeMCPSnapshotOptions(opts snapshot.SnapshotOptions) snapshot.SnapshotOptions {
	// Shared with the HTTP surface so both transports default identically.
	return snapshot.NormalizeOptions(opts)
}

func normalizeMCPFindOptions(opts snapshot.FindOptions) snapshot.FindOptions {
	if opts.Limit <= 0 {
		opts.Limit = defaultFindLimit
	}
	return opts
}

func parseMouseButtonArgs(args json.RawMessage) (browser.MouseButtonOptions, error) {
	var req struct {
		Ref    string   `json:"ref"`
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
		Button string   `json:"button"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return browser.MouseButtonOptions{}, err
	}
	return browser.MouseButtonOptions{
		MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
		Button:     req.Button,
	}, nil
}

func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	return json.Unmarshal(args, dst)
}

// tabIDArg reconciles the historical `id` parameter of brw_focus_tab /
// brw_close_tab with the `tab_id` parameter every other page tool uses.
// Callers that pass {tab_id:"..."} (consistent with the rest of the surface)
// were previously silently ignored, leaving an empty id that the extension
// bridge coerced to tab 0. Prefer `tab_id`, fall back to `id` for backward
// compatibility.
// tabIDArg resolves the canonical tab id from the preferred tab_id field and its
// deprecated id alias (brw_focus_tab / brw_close_tab). Precedence is
// intentional graceful promotion: a non-empty tab_id always wins, and id is used
// only as a fallback. If a caller supplies both with different values, tab_id is
// used and id is silently ignored — documented in the tool schemas where id is
// labelled "Deprecated alias for tab_id".
func tabIDArg(tabID, id string) string {
	if strings.TrimSpace(tabID) != "" {
		return tabID
	}
	return id
}

func contextIDArg(contextID, legacyBrowserContextID string) string {
	if strings.TrimSpace(contextID) != "" {
		return contextID
	}
	return legacyBrowserContextID
}

// defaultEvaluateMaxBytes bounds the serialized brw_evaluate result returned to
// the client. Historically an oversized result came back EMPTY (the payload was
// silently dropped past ~11KB by a downstream size limit); we now truncate with
// an explicit marker so the caller always gets the leading bytes plus the total
// length, and can page through the rest with offset/max_bytes.
const defaultEvaluateMaxBytes = 64 * 1024

// evaluateResult serializes a brw_evaluate value and applies offset/max_bytes
// windowing. An oversized window is truncated with an explicit suffix marker
// (never returned empty). offset/max_bytes are clamped to sane ranges; passing
// neither yields the leading defaultEvaluateMaxBytes of the result.
func evaluateResult(value any, err error, offset, maxBytes int) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return toolError(marshalErr), nil
	}

	total := len(data)
	if offset < 0 {
		offset = 0
	}
	if maxBytes <= 0 {
		maxBytes = defaultEvaluateMaxBytes
	}

	// offset past the end yields an explicit empty window, not a confusing nil.
	if offset >= total {
		text := fmt.Sprintf("…[truncated: offset %d is at or beyond end; returned 0 of %d bytes]", offset, total)
		return map[string]any{"content": []toolContent{{Type: "text", Text: text}}}, nil
	}

	// Clamp the window WITHOUT overflowing offset+maxBytes: max_bytes is
	// caller-controlled and could be near math.MaxInt, which would wrap
	// negative and panic data[offset:end]. offset < total is guaranteed above,
	// so total-offset is a safe positive bound.
	end := total
	if maxBytes < total-offset {
		end = offset + maxBytes
	}
	window := string(data[offset:end])
	truncated := offset > 0 || end < total

	if !truncated {
		// Small (or fully-covered) result: behave exactly like toolJSON so
		// structured clients still get structuredContent for object payloads.
		result := map[string]any{
			"content": []toolContent{{Type: "text", Text: window}},
		}
		if isJSONObject(data) {
			result["structuredContent"] = value
		}
		return result, nil
	}

	marker := fmt.Sprintf("\n…[truncated: returned %d of %d bytes (offset %d); request more with offset=%d, max_bytes=N]",
		end-offset, total, offset, end)
	return map[string]any{
		"content": []toolContent{{Type: "text", Text: window + marker}},
	}, nil
}

func toolJSON[T any](value T, err error) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return toolError(err), nil
	}
	result := map[string]any{
		"content": []toolContent{{Type: "text", Text: string(data)}},
	}
	// Per MCP, structuredContent MUST be a JSON object. Tools whose payload can
	// be an array or scalar — notably brw_evaluate returning a string/number,
	// or list tools returning a top-level array — would otherwise emit a
	// non-object structuredContent that strict clients reject
	// with an "expected record" schema error, forcing wasteful retries. Only
	// attach structuredContent when the payload actually serializes to an object;
	// the text content always carries the full result regardless.
	if isJSONObject(data) {
		result["structuredContent"] = value
	}
	return result, nil
}

// isJSONObject reports whether data is a JSON object (ignoring leading whitespace).
func isJSONObject(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
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

func canonicalToolName(name string) string {
	if strings.HasPrefix(name, "browser_") {
		return "brw_" + strings.TrimPrefix(name, "browser_")
	}
	return name
}

func tools() []map[string]any {
	return []map[string]any{
		tool("brw_open", "Open a URL in a visible Chrome/Chromium tab. To start a run-scoped tab group, pass a unique group name such as workspace-1; to add later tabs to that same visible group, pass its group_id from brw_list_tabs or brw_list_tab_groups.", object(map[string]any{
			"url":         stringSchema("URL to open. Scheme defaults to https."),
			"group":       stringSchema("Optional Chrome tab group title. When set without group_id, the extension reuses an existing same-title group in the target window or creates one."),
			"group_id":    stringSchema("Optional existing Chrome tab group id from brw_list_tabs or brw_list_tab_groups. When set, the new tab is added to that group."),
			"group_color": stringSchema("Optional group color: grey, blue, red, yellow, green, pink, purple, cyan, orange."),
		}, []string{"url"})),
		tool("brw_open_incognito", "Open a URL in a brand-new INCOGNITO browser context: a fully isolated session with its own cookies, storage, and cache that shares nothing with the normal profile or other contexts (the CDP equivalent of an incognito window). Returns the new tab including its context_id. WHEN DONE, call brw_close_context with that context_id to dispose the whole context (closes every tab in it and discards its data). DIRECT-CDP TRANSPORT ONLY: on the extension-bridge transport (driving the user's existing signed-in Chrome) this returns an error — use a dedicated direct-CDP profile for incognito. Ideal for clean-room / logged-out internal testing.", object(map[string]any{
			"url": stringSchema("URL to open in the new incognito context. Scheme defaults to https."),
		}, []string{"url"})),
		tool("brw_close_context", "Dispose an incognito browser context created by brw_open_incognito: closes every tab inside it and discards its isolated cookies/storage. Pass the context_id returned by brw_open_incognito. Direct-CDP transport only.", object(map[string]any{
			"context_id":         stringSchema("The context_id returned by brw_open_incognito."),
			"browser_context_id": stringSchema("Deprecated alias for context_id."),
		}, []string{"context_id"})),
		tool("brw_list_tabs", "List controllable Chrome/Chromium browser targets, including tabs, popup windows, and Chrome tab-group metadata when the extension bridge reports it. Ungrouped/default tabs remain listed normally.", object(nil, nil)),
		tool("brw_list_tab_groups", "List visible Chrome tab groups with ids, titles, colors, collapsed state, window ids, and member tab ids. Extension-bridge transport only; direct CDP cannot inspect Chrome tab groups.", object(nil, nil)),
		tool("brw_focus_tab", "Focus a controllable Chrome/Chromium target and make it the default target for following reads/actions.", object(map[string]any{
			"tab_id": stringSchema("Target id from brw_list_tabs (preferred, consistent with other tools)."),
			"id":     stringSchema("Deprecated alias for tab_id."),
		}, nil)),
		tool("brw_close_tab", "Close a controllable Chrome/Chromium target.", object(map[string]any{
			"tab_id": stringSchema("Target id from brw_list_tabs (preferred, consistent with other tools)."),
			"id":     stringSchema("Deprecated alias for tab_id."),
		}, nil)),
		tool("brw_read", "Return semantic page content: main text, headings, links, forms, tables, and metadata. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_read_data", "Extract embedded structured page data (Next.js __NEXT_DATA__, JSON-LD, microdata, Open Graph) as a compact normalized object without DOM rendering. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_snapshot", "Return interactive controls with stable refs. Defaults to a bounded visible/actionable viewport frontier; use mode:\"all\" for full-page debugging (returns every matching element including offscreen/hidden controls — useful for comprehensive page analysis), and add include_hidden:true only when hidden inputs are needed. Metadata includes total_candidates for the full count before filtering. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id":               stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
			"mode":                 stringSchema("frontier (default, scored visible/actionable controls) or all (full matching list, including offscreen/currently invisible matching controls) or form_lens (form fields with validation state only)."),
			"query":                stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value."),
			"text":                 stringSchema("Alias for query-style text filtering."),
			"role":                 stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":                integerSchema("Maximum number of elements to return. Defaults to 40 in frontier mode."),
			"viewport_only":        boolSchema("Only return elements intersecting the viewport. Forced true in default frontier mode."),
			"include_hidden":       boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
			"include_ax":           boolSchema("Include full accessibility-tree enrichment. Expensive; defaults false."),
			"visual_islands":       boolSchema("Detect semantically-opaque visual content (canvas/svg/video/large image/background-image/custom-rendered widget) and emit each as an element with source:[\"visual\"], visual_type, and visual_hint. Off by default; islands compete with DOM elements in the merged list up to the limit, so dense pages stay token-efficient."),
			"visual_islands_limit": integerSchema("Cap on detected visual islands before merging into the element list. Defaults to 10."),
			"since":                integerSchema("Pass a prior snapshot's metadata.version to get a DELTA: when it matches the last snapshot taken with identical options, the response sets metadata.delta=true, 'elements' carries ONLY added+changed elements (a change set, not the full page), and a top-level 'delta' object lists {added, removed, changed} refs (removed = refs whose element left the DOM). On any mismatch (version, options, or after navigation) a normal full snapshot is returned. Omit for a full snapshot."),
		}, nil)),
		tool("brw_find", "Find matching semantic element refs without dumping the full page. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id":         stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
			"query":          stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value. Set text_content:true to also match visible prose text."),
			"text":           stringSchema("Alias for query-style text filtering."),
			"role":           stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":          integerSchema("Maximum number of elements to return."),
			"viewport_only":  boolSchema("Only return elements intersecting the viewport."),
			"include_hidden": boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
			"text_content":   boolSchema("Also match against full visible text content (innerText), surfacing prose-bearing elements like headings, paragraphs, and list items — not just interactive-element metadata. Opt-in; defaults false."),
		}, nil)),
		tool("brw_click", "Click a semantic element ref (or x,y coordinates) from brw_snapshot. Defaults to a left single-click; set button to right (opens context menus) or middle, and click_count to 2 (double-click) or 3 (triple-click selects a line). Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":         stringSchema("Element ref, for example e18. Provide ref or x,y."),
			"x":           map[string]any{"type": "number", "description": "X coordinate in viewport pixels. Use with y instead of ref for canvas/coordinate clicks."},
			"y":           map[string]any{"type": "number", "description": "Y coordinate in viewport pixels. Use with x instead of ref for canvas/coordinate clicks."},
			"button":      stringSchema("Mouse button: left (default), right, or middle."),
			"click_count": integerSchema("Click count: 1 (default), 2 for double-click, 3 to triple-click (select a line)."),
			"tab_id":      stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_drag", "Press at a source (ref or x,y), move to a target (ref or x,y) over several steps, then release. Use for sliders/range inputs, drag-and-drop reorder, and canvas/map panning. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"from":   mousePointSchema("Drag source. Provide either ref or x and y."),
			"to":     mousePointSchema("Drag target. Provide either ref or x and y."),
			"steps":  integerSchema("Number of intermediate mouse-move steps between source and target. Defaults to 12."),
			"button": stringSchema("Mouse button held during the drag: left (default), right, or middle."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"from", "to"})),
		tool("brw_mouse_down", "Press and hold a mouse button at a ref or x,y without releasing (the press half of a press-and-hold). Pair with brw_mouse_up. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref to press at. Provide ref or x,y."),
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"button": stringSchema("Mouse button: left (default), right, or middle."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_mouse_up", "Release a held mouse button at a ref or x,y (the release half of a press-and-hold). Pair with brw_mouse_down. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref to release at. Provide ref or x,y."),
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"button": stringSchema("Mouse button: left (default), right, or middle."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_click_text", "Click the best visible actionable element whose accessible name or visible text matches text. Useful for controls like \"Check out\" when refs are stale or custom components hide internals. Below-fold matches are scrolled into view before clicking by default. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"text":        stringSchema("Visible text or accessible name to click."),
			"role":        stringSchema("Optional role filter, for example button, link, option, or menuitem."),
			"exact":       boolSchema("Require an exact normalized text/name match instead of allowing substring matches."),
			"auto_scroll": boolSchema("Scroll a below-fold match into view before clicking (default true). Set false to click only elements already in the viewport."),
			"tab_id":      stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"text"})),
		tool("brw_navigate", "Navigate the active tab's session history: back, forward, or reload. Uses the page navigation history (no URL needed); returns a post-navigation observation. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"direction": stringSchema("back (previous history entry), forward (next history entry), or reload (re-fetch the current document)."),
			"tab_id":    stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"direction"})),
		tool("brw_hover", "Hover over a semantic element ref to trigger mouseenter/mouseover/pointermove events. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref, for example e18."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"ref"})),
		tool("brw_evaluate", "Run arbitrary JavaScript in the page context and return the JSON-serializable result. Supports async expressions. Pass optional tab_id to target a specific tab. Large results are TRUNCATED with an explicit '…[truncated: returned N of M bytes]' marker (never silently empty); use offset/max_bytes to page through them. Note: fetch() runs under the current page's Content-Security-Policy, so cross-origin calls must be made from a tab whose origin permits them (otherwise they fail with a CSP/'Failed to fetch' error).", object(map[string]any{
			"expression": stringSchema("JavaScript expression to evaluate. May use await for async operations."),
			"offset":     map[string]any{"type": "integer", "description": "Byte offset into the serialized result to start returning from. Defaults to 0. Use with the marker on a truncated response to page forward."},
			"max_bytes":  map[string]any{"type": "integer", "description": "Maximum bytes of the serialized result to return in this call. Defaults to 65536. The response is truncated (with a marker) rather than dropped when the result is larger."},
			"tab_id":     stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"expression"})),
		tool("brw_network_requests", "Return network resource requests captured by the Performance API (performance.getEntriesByType). Pass optional tab_id to target a specific tab.", object(map[string]any{
			"filter": stringSchema("Optional case-insensitive substring to filter request URLs."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_network_capture", "Install an idempotent in-page interceptor wrapping fetch and XMLHttpRequest, then drain and return recently captured requests (method, url, request headers/body, status, ok, response snippet, started_at, duration_ms). Works on both transports because capture is pure in-page JS (no CDP Network domain required). Bodies and response snippets are truncated. Call once to start capturing, then again after triggering page activity to read what was recorded. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"filter": stringSchema("Optional case-insensitive substring to filter captured request URLs."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_replay_request", "Re-execute a request in-page via fetch(url, {method, headers, body}) and return {status, ok, body}. SAFETY: a MUTATING replay (POST/PUT/PATCH/DELETE) whose URL looks like checkout, payment, purchase, or order placement is BLOCKED with an error and never executed; idempotent GET/HEAD reads are always allowed. Use to re-run safe read/idempotent API calls (for example a GET) discovered via brw_network_capture. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"method":  stringSchema("HTTP method, for example GET or POST. Defaults to GET."),
			"url":     stringSchema("Request URL. May be relative to the current page."),
			"headers": map[string]any{"type": "object", "description": "Optional request headers as a string-to-string map.", "additionalProperties": stringSchema("Header value.")},
			"body":    stringSchema("Optional request body. Ignored for GET/HEAD."),
			"tab_id":  stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"url"})),
		tool("brw_type", "Type text into a semantic element ref. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref, for example e17."),
			"text":   stringSchema("Text to insert."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"ref", "text"})),
		tool("brw_fill", "Replace or append text in a semantic text field by ref or query and return a post-action observation. Also sets a native range slider (<input type=range>), number, or date input to an exact value in ONE call (prefer this over repeated brw_press arrow keys for sliders). Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":     stringSchema("Element ref, for example e17. Optional when query is supplied."),
			"query":   stringSchema("Find a fillable target by semantic name when ref is not supplied."),
			"role":    stringSchema("Optional role filter when using query, normally textbox or searchbox."),
			"text":    stringSchema("Text to put in the field."),
			"replace": boolSchema("Replace existing field content instead of appending. Defaults to true."),
			"tab_id":  stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"text"})),
		tool("brw_upload_file", "Set a file on a semantic file input by ref or query and return a post-action observation. Provide the file from EXACTLY ONE source: path/paths (files already on the browser host), bytes_base64 (inline base64 contents — the daemon writes them to a temp file for you, no host filesystem access needed), or url (the daemon fetches it over http(s) to a temp file). Temp files created from bytes_base64/url are removed after the upload. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":          stringSchema("Element ref for input[type=file]. Optional when query is supplied."),
			"query":        stringSchema("Find a file input by semantic name when ref is not supplied. Defaults to file."),
			"role":         stringSchema("Optional role filter when using query."),
			"path":         stringSchema("Single local file path on the browser host. One of path/paths, bytes_base64, or url."),
			"paths":        map[string]any{"type": "array", "items": stringSchema("Local file path on the browser host."), "description": "One or more local file paths on the browser host. One of path/paths, bytes_base64, or url."},
			"bytes_base64": stringSchema("Inline file contents as a standard base64 string. The daemon decodes and writes them to a temp file on the browser host. Use filename to control the name the page sees. One of path/paths, bytes_base64, or url."),
			"url":          stringSchema("http(s) URL the daemon fetches to a temp file on the browser host before uploading. One of path/paths, bytes_base64, or url."),
			"filename":     stringSchema("Optional name for the temp file created from bytes_base64 or url (the page sees this basename). Defaults to the url basename or a generic name."),
			"tab_id":       stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_select", "Set a native select or custom listbox/combobox value by semantic element ref. Value may be the option value/data-value or visible option label. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref for a select, combobox, or listbox trigger."),
			"value":  stringSchema("Option value, data-value, or visible option label to select."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"ref", "value"})),
		tool("brw_press", "Press a keyboard key in the active tab. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"key":    stringSchema("Key name or chord, for example Enter, Tab, Escape, ArrowDown, Meta+Enter."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"key"})),
		tool("brw_scroll", "Scroll the active page or scroll container in a direction. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"direction": stringSchema("up, down, left, or right."),
			"tab_id":    stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"direction"})),
		tool("brw_screenshot", "Visual fallback — you almost never need this. brw is semantic-first: brw_snapshot/brw_find expose every control with a ref, brw_read returns page prose/result/status/badge text, and EVERY action (click/type/fill/select/press/drag) returns a post-action observation that confirms its effect (changed elements, new values, navigation). To VERIFY an outcome (a cart badge, a result message, a swapped item, an editor's text), read that observation or call brw_read — do NOT screenshot to check. Reserve brw_screenshot for opaque visual content with no DOM text (canvas, maps, charts, image-only widgets). Pass optional tab_id to target a specific tab. Set annotate:true for a Set-of-Marks capture: each in-viewport frontier element is drawn with a labelled box whose label is the SAME ref returned by brw_snapshot (e.g. e17), and the response carries a legend mapping each ref to its box (x,y,width,height) plus role and name — so a vision model can read a label off the image and act on it with brw_click using that exact ref. To save vision tokens on a dense page, pass ref OR region to get a TIGHT annotated crop of just that element / box instead of the whole viewport (a far smaller image); ref/region imply annotate. The overlay is removed immediately after capture and never mutates the page. Default (annotate omitted/false, no ref/region) is byte-identical to the plain capture.", object(map[string]any{
			"tab_id":   stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
			"annotate": boolSchema("Draw Set-of-Marks ref labels over frontier elements and return a ref->box legend. Defaults false (plain screenshot)."),
			"ref":      stringSchema("Optional element ref from brw_snapshot. Returns a tight annotated crop clipped to that element's box (smaller image, fewer vision tokens). Implies annotate."),
			"region": map[string]any{
				"type":        "object",
				"description": "Optional viewport-space clip rectangle for a tight annotated crop (in CSS pixels). Implies annotate. Use when you know the box of the visual island you want to inspect.",
				"properties": map[string]any{
					"x":      map[string]any{"type": "number", "description": "Left edge in viewport pixels."},
					"y":      map[string]any{"type": "number", "description": "Top edge in viewport pixels."},
					"width":  map[string]any{"type": "number", "description": "Clip width in pixels."},
					"height": map[string]any{"type": "number", "description": "Clip height in pixels."},
				},
			},
		}, nil)),
		tool("brw_screenshot_element", "Capture a PNG screenshot of a semantic element ref for visual fallback/debugging. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"ref":    stringSchema("Element ref from brw_snapshot."),
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"ref"})),
		tool("brw_wait_for", "Wait for page readiness, URL/title/text substring, or ref availability. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"condition":  stringSchema("ready (document interactive/complete), load (alias of ready), committed (interactive/complete AND a real navigated URL, not about:blank), text:..., not_text:..., url:..., not_url:..., title:..., ref:..., or plain text."),
			"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in milliseconds."},
			"tab_id":     stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"condition"})),
		tool("brw_plan", "Execute a sequence of browser operations in one round-trip. Steps run sequentially and stop on first failure.", object(map[string]any{
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
		tool("brw_batch", "Execute multiple browser actions in one round-trip without intermediate observations. Returns a single compact observation at the end. Supports actions (click, type, fill, select, press, scroll, hover, wait, open, focus_tab) and inline assertions (assert_visible, assert_text, assert_value, assert_hidden).", object(map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered list of actions and assertions to execute.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":     stringSchema("One of: click, type, fill, select, press, scroll, hover, wait, open, focus_tab, assert_visible, assert_text, assert_value, assert_hidden."),
						"ref":        stringSchema("Element ref for click, type, fill, select, hover, and assert_* actions."),
						"text":       stringSchema("Text for type and fill actions, or expected text for assert_text."),
						"value":      stringSchema("Option value for select action, or expected value for assert_value."),
						"direction":  stringSchema("Scroll direction: up, down, left, right."),
						"condition":  stringSchema("Wait condition (load, text:..., ref:..., url:..., etc)."),
						"timeout_ms": map[string]any{"type": "integer", "description": "Timeout for wait/assert actions in milliseconds."},
						"url":        stringSchema("URL for open action."),
						"id":         stringSchema("Tab id for focus_tab action."),
						"key":        stringSchema("Key name for press action (Enter, Tab, Escape, etc)."),
					},
					"required": []string{"action"},
				},
			},
		}, []string{"steps"})),
		tool("brw_cancel", "Cooperatively stop in-flight long-running operations (brw_plan, brw_batch, and their waits) for an operation token. Omit token (or pass \"*\") to stop everything; pass tab_id to stop work targeting that tab. The cancelled operation returns a normal result reporting steps_completed and cancelled=true rather than erroring. Returns how many operations were signalled.", object(map[string]any{
			"token":  stringSchema("Operation token to cancel. Omit or use \"*\" to cancel all in-flight operations."),
			"tab_id": stringSchema("Optional tab id. When set (and no explicit token), cancels operations targeting that tab."),
		}, nil)),
		tool("brw_observe", "Return compact page state: version, URL, title, focused ref, and frontier element changes since last observe. Use this to check what changed without a full snapshot. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_group_tabs", "Group tabs into a named Chrome tab group, or move them into an existing group_id.", object(map[string]any{
			"tab_ids":  map[string]any{"type": "array", "items": stringSchema("Tab id."), "description": "Tab IDs to group."},
			"name":     stringSchema("Group name shown in Chrome tab strip. Used when creating/reusing by title, or renaming a group_id target."),
			"color":    stringSchema("Group color: grey, blue, red, yellow, green, pink, purple, cyan, orange."),
			"group_id": stringSchema("Optional existing Chrome tab group id. When set, the tabs are moved into that group."),
		}, []string{"tab_ids"})),
		tool("brw_ungroup_tabs", "Remove tabs from their Chrome tab group.", object(map[string]any{
			"tab_ids": map[string]any{"type": "array", "items": stringSchema("Tab id."), "description": "Tab IDs to ungroup."},
		}, []string{"tab_ids"})),
		tool("brw_assert_visible", "Assert that an element ref is visible. Retries until visible or timeout (web-first assertion).", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
		}, []string{"ref"})),
		tool("brw_assert_text", "Assert that an element ref contains the expected text (case-insensitive substring). Retries until matched or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"text":       stringSchema("Expected text substring (case-insensitive)."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
		}, []string{"ref", "text"})),
		tool("brw_assert_value", "Assert that an element ref has the expected value (exact match). Retries until matched or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"value":      stringSchema("Expected value (exact match)."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
		}, []string{"ref", "value"})),
		tool("brw_assert_hidden", "Assert that an element ref is hidden or absent from the DOM. Retries until hidden or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
		}, []string{"ref"})),
		tool("brw_commit", "Commit a form field: submits the enclosing form (via submit button or requestSubmit) or presses Enter if no form. Use after filling a field that requires explicit submission.", object(map[string]any{
			"ref": stringSchema("Element ref from brw_snapshot."),
		}, []string{"ref"})),
		tool("brw_notify", "Raise a desktop notification to pull the human operator back at a hand-off point (needs_input for MFA/CAPTCHA/purchase confirmation), on completion (done), or on failure (error) — useful when the user has tabbed away. With the Chrome extension bridge this uses chrome.notifications and surfaces even when the tab is backgrounded; on a direct-CDP session it falls back to the in-page Notification API (best-effort, subject to page focus/permission). The result reports the honest delivery channel (extension, page, or unavailable).", object(map[string]any{
			"kind":    stringSchema("Hand-off classification: needs_input (default), done, or error."),
			"title":   stringSchema("Short notification heading. Defaults to a kind-appropriate title."),
			"message": stringSchema("Notification body text."),
		}, nil)),
		tool("brw_click_xy", "Click at specific viewport coordinates (x, y). Returns the element that was clicked. Use for canvas interactions or when semantic refs are not available. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, []string{"x", "y"})),
		tool("brw_console", "Return and drain buffered console messages (log, warn, error, info) from the page. Messages are captured by an injected console interceptor and cleared after reading. Pass optional tab_id to target a specific tab.", object(map[string]any{
			"tab_id": stringSchema("Optional tab id from brw_list_tabs. Omit to use the active tab."),
		}, nil)),
		tool("brw_downloads", "Return and drain tracked file downloads. Download capture is enabled lazily on first call (Browser.setDownloadBehavior with events); subsequent triggered downloads are recorded via the Browser.downloadWillBegin/downloadProgress CDP events with url, suggested_filename, state (inProgress/completed/canceled), received_bytes, total_bytes, guid, and path. The buffer is cleared after reading. The result carries supported=true on the direct-CDP backend. Over the extension bridge it returns an empty list with supported=false plus an explanatory note (the bridge cannot observe CDP download events); branch on supported to detect this case.", object(nil, nil)),
		tool("brw_trace", "Return the action trace: a compact log of recent actions with refs, timing, and outcomes. Use for debugging and performance analysis.", object(nil, nil)),
		tool("brw_clear_trace", "Clear the action trace buffer.", object(nil, nil)),
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

// mousePointSchema describes a drag endpoint: a semantic ref OR x,y coordinates.
func mousePointSchema(description string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": description,
		"properties": map[string]any{
			"ref": stringSchema("Element ref, for example e18."),
			"x":   map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":   map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
		},
	}
}
