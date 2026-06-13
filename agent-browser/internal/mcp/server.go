package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
)

type Server struct {
	manager *browser.Manager
}

func New(manager *browser.Manager) *Server {
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
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	encoder := json.NewEncoder(out)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		result, rpcErr := s.handle(ctx, req.Method, req.Params)
		resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}
		if err := encoder.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
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
			URL string `json:"url"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Open(ctx, req.URL))
	case "browser_read":
		return toolJSON(s.manager.Read(ctx))
	case "browser_snapshot":
		return toolJSON(s.manager.Snapshot(ctx))
	case "browser_click":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.Click(ctx, req.Ref))
	case "browser_type":
		var req struct {
			Ref  string `json:"ref"`
			Text string `json:"text"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.Type(ctx, req.Ref, req.Text))
	case "browser_press":
		var req struct {
			Key string `json:"key"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.Press(ctx, req.Key))
	case "browser_screenshot":
		shot, err := s.manager.Screenshot(ctx)
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
	default:
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", name)}
	}
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
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{"content": []toolContent{{Type: "text", Text: string(data)}}}, nil
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
			"url": stringSchema("URL to open. Scheme defaults to https."),
		}, []string{"url"})),
		tool("browser_read", "Return semantic page content: main text, headings, links, forms, tables, and metadata.", object(nil, nil)),
		tool("browser_snapshot", "Return visible interactive controls with stable refs from DOM and accessibility data.", object(nil, nil)),
		tool("browser_click", "Click a semantic element ref from browser_snapshot.", object(map[string]any{
			"ref": stringSchema("Element ref, for example e18."),
		}, []string{"ref"})),
		tool("browser_type", "Type text into a semantic element ref.", object(map[string]any{
			"ref":  stringSchema("Element ref, for example e17."),
			"text": stringSchema("Text to insert."),
		}, []string{"ref", "text"})),
		tool("browser_press", "Press a keyboard key in the active tab.", object(map[string]any{
			"key": stringSchema("Key name, for example Enter, Tab, Escape, ArrowDown."),
		}, []string{"key"})),
		tool("browser_screenshot", "Capture a PNG screenshot for visual fallback/debugging.", object(nil, nil)),
		tool("browser_wait_for", "Wait for page readiness, URL/title/text substring, or ref availability.", object(map[string]any{
			"condition":  stringSchema("load, text:..., url:..., title:..., ref:..., or plain text."),
			"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in milliseconds."},
		}, []string{"condition"})),
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
