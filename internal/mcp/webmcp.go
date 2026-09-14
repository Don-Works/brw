package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// WebMCP page-tool dispatch.
//
// brw_call_page_tool started as a single blocking call, which works only for a
// tool that returns quickly. A page tool is a function inside a document and the
// page decides how long it runs; anything slower than the caller's patience left
// the agent with no result AND no handle on work that was still running.
//
// So the three verbs are split the way a job queue splits them: start (detached
// or waited on), collect by id, cancel by id. The invocation registry lives in
// the page (internal/snapshot/webmcp_invoke.go), reached through Evaluate, which
// is why every transport gets this without a second implementation.

// pageToolEvaluator adapts the controller's Evaluate to the page-tool runner.
func (s *Server) pageToolEvaluator() snapshot.PageToolEvaluator {
	return func(ctx context.Context, expression string) (any, error) {
		return s.manager.Evaluate(ctx, expression)
	}
}

// pageToolTimeout clamps a caller's timeout_ms. A zero or missing value means
// fallback; a negative one is a mistake, not a request for no wait at all.
func pageToolTimeout(ms int, fallback time.Duration) time.Duration {
	if ms == 0 {
		return fallback
	}
	if ms < 0 {
		return 0
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout > snapshot.MaxPageToolTimeout {
		return snapshot.MaxPageToolTimeout
	}
	return timeout
}

func (s *Server) callPageTool(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Frame     string          `json:"frame"`
		Detach    bool            `json:"detach"`
		TimeoutMS int             `json:"timeout_ms"`
		// ValidateInput defaults to true, so the pointer distinguishes "not
		// supplied" from an explicit false.
		ValidateInput *bool `json:"validate_input"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	if strings.TrimSpace(req.Name) == "" {
		return toolError(errors.New("name is required; call brw_page_tools to list available page tools")), nil
	}
	validate := true
	if req.ValidateInput != nil {
		validate = *req.ValidateInput
	}
	return toolJSON(snapshot.InvokePageTool(ctx, s.pageToolEvaluator(), snapshot.PageToolInvokeOptions{
		Name:      req.Name,
		Arguments: req.Arguments,
		Frame:     req.Frame,
		Detach:    req.Detach,
		Timeout:   pageToolTimeout(req.TimeoutMS, snapshot.DefaultPageToolTimeout),
		Validate:  validate,
	}))
}

func (s *Server) pageToolResult(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		InvocationID string `json:"invocation_id"`
		ID           string `json:"id"`
		TimeoutMS    int    `json:"timeout_ms"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	id := firstNonEmpty(req.InvocationID, req.ID)
	// No timeout means "tell me where it is now", which is the cheap poll an
	// agent interleaves with other work.
	return toolJSON(snapshot.AwaitPageTool(ctx, s.pageToolEvaluator(), id, pageToolTimeout(req.TimeoutMS, 0)))
}

func (s *Server) cancelPageTool(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		InvocationID string `json:"invocation_id"`
		ID           string `json:"id"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	return toolJSON(snapshot.CancelPageTool(ctx, s.pageToolEvaluator(), firstNonEmpty(req.InvocationID, req.ID)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
