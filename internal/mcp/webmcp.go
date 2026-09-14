package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
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
// label names the call in the trace instead of the generated script: a bounded
// wait runs one evaluate per poll — hundreds of them at the ten-minute cap — and
// recorded as raw evaluate rows they would push the session's real activity out
// of the trace ring.
func (s *Server) pageToolEvaluator(label string) snapshot.PageToolEvaluator {
	return func(ctx context.Context, expression string) (any, error) {
		return s.manager.Evaluate(browser.WithTraceLabel(ctx, browser.TraceActionPageTool, label), expression)
	}
}

// pageToolTimeout clamps a caller's timeout_ms. Zero or missing means the calling
// tool's own default. A negative value is refused rather than guessed at: the two
// readings of it — no wait at all, or the default wait — differ by thirty seconds
// of the agent's turn, and neither is what the caller asked for.
func pageToolTimeout(ms int, fallback time.Duration) (time.Duration, error) {
	if ms < 0 {
		return 0, fmt.Errorf("timeout_ms must not be negative, got %d; omit it for this tool's default wait", ms)
	}
	if ms == 0 {
		return fallback, nil
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout > snapshot.MaxPageToolTimeout {
		return snapshot.MaxPageToolTimeout, nil
	}
	return timeout, nil
}

func (s *Server) callPageTool(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Frame     string          `json:"frame"`
		Detach    bool            `json:"detach"`
		TimeoutMS int             `json:"timeout_ms"`
		Offset    int             `json:"offset"`
		MaxBytes  int             `json:"max_bytes"`
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
	timeout, err := pageToolTimeout(req.TimeoutMS, snapshot.DefaultPageToolTimeout)
	if err != nil {
		return toolError(err), nil
	}
	validate := true
	if req.ValidateInput != nil {
		validate = *req.ValidateInput
	}
	invocation, err := snapshot.InvokePageTool(ctx, s.pageToolEvaluator("call "+strings.TrimSpace(req.Name)), snapshot.PageToolInvokeOptions{
		Name:      req.Name,
		Arguments: req.Arguments,
		Frame:     req.Frame,
		Detach:    req.Detach,
		Timeout:   timeout,
		Validate:  validate,
	})
	return pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
}

func (s *Server) pageToolResult(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		InvocationID string `json:"invocation_id"`
		TimeoutMS    int    `json:"timeout_ms"`
		Offset       int    `json:"offset"`
		MaxBytes     int    `json:"max_bytes"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	// No timeout means "tell me where it is now", which is the cheap poll an
	// agent interleaves with other work.
	timeout, err := pageToolTimeout(req.TimeoutMS, 0)
	if err != nil {
		return toolError(err), nil
	}
	id := strings.TrimSpace(req.InvocationID)
	invocation, err := snapshot.AwaitPageTool(ctx, s.pageToolEvaluator("result "+id), id, timeout)
	return pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
}

func (s *Server) cancelPageTool(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req struct {
		InvocationID string `json:"invocation_id"`
		Offset       int    `json:"offset"`
		MaxBytes     int    `json:"max_bytes"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	id := strings.TrimSpace(req.InvocationID)
	invocation, err := snapshot.CancelPageTool(ctx, s.pageToolEvaluator("cancel "+id), id)
	return pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
}

// pageToolReport renders one invocation as a bounded tool result.
//
// The payload is a page tool's own return value, so the page decides its size: it
// goes through the same offset/max_bytes windowing as brw_evaluate rather than
// being serialised whole into the turn, and a cached result stays collectable for
// five minutes, so an unbounded one could be re-dumped repeatedly.
//
// A wait that ended early — a cancelled request, a closed tab, an evaluate that
// failed — is reported WITH its id rather than as a bare error, because the
// invocation is still running in the document whatever happened to the call that
// was watching it, and the id is the only way back to it.
func pageToolReport(ctx context.Context, invocation snapshot.PageToolInvocation, err error, offset, maxBytes int) (any, *rpcError) {
	if err != nil && invocation.ID == "" {
		return toolError(err), nil
	}
	if err != nil {
		invocation.OK = false
		invocation.Error = err.Error()
	}
	if invocation.TabID == "" {
		invocation.TabID = browser.TabIDFromContext(ctx)
	}
	return evaluateResult(invocation, nil, offset, maxBytes)
}
