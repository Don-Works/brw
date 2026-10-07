package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (s *Server) pageToolEvaluator(label string) snapshot.PageToolEvaluator {
	return func(ctx context.Context, expression string) (any, error) {
		return s.manager.Evaluate(browser.WithTraceLabel(ctx, browser.TraceActionPageTool, label), expression)
	}
}

const pageToolTabLookupTimeout = 5 * time.Second

func pageToolTimeout(ms int, fallback time.Duration) (time.Duration, error) {
	if ms < 0 {
		return 0, fmt.Errorf("timeout_ms must not be negative, got %d; omit it for this tool's default wait", ms)
	}
	if ms == 0 {
		return fallback, nil
	}
	if ms > int(snapshot.MaxPageToolTimeout/time.Millisecond) {
		return snapshot.MaxPageToolTimeout, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
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
		// ValidateInput defaults to true, so the pointer distinguishes "not supplied" from an explicit false.
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
	if err := s.confirmPageTool(ctx, strings.TrimSpace(req.Name), req.Frame); err != nil {
		return toolError(err), nil
	}
	invocation, err := snapshot.InvokePageTool(ctx, s.pageToolEvaluator("call "+strings.TrimSpace(req.Name)), snapshot.PageToolInvokeOptions{
		Name:      req.Name,
		Arguments: req.Arguments,
		Frame:     req.Frame,
		Detach:    req.Detach,
		Timeout:   timeout,
		Validate:  validate,
	})
	return s.pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
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

	timeout, err := pageToolTimeout(req.TimeoutMS, 0)
	if err != nil {
		return toolError(err), nil
	}
	id := strings.TrimSpace(req.InvocationID)
	invocation, err := snapshot.AwaitPageTool(ctx, s.pageToolEvaluator("result "+id), id, timeout)
	return s.pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
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
	return s.pageToolReport(ctx, invocation, err, req.Offset, req.MaxBytes)
}

func (s *Server) pageToolReport(ctx context.Context, invocation snapshot.PageToolInvocation, err error, offset, maxBytes int) (any, *rpcError) {
	if err != nil && invocation.ID == "" {
		return toolError(err), nil
	}
	if err != nil {
		invocation.OK = false
		invocation.Error = err.Error()
	}
	if invocation.TabID == "" {
		invocation.TabID = s.pageToolTabID(ctx)
	}
	if len(invocation.Result) > 0 {
		invocation.UntrustedOutput = true
	}
	return evaluateResult(invocation, nil, offset, maxBytes)
}

func (s *Server) confirmPageTool(ctx context.Context, name, frame string) error {
	if approval.IsExecution(ctx) {
		return nil
	}
	if !s.consent.Enabled() || !s.consent.ConfirmActions() {
		return nil
	}
	listing, err := snapshot.ListPageTools(ctx, s.pageToolEvaluator("list "+name), frame)
	if err != nil {
		return fmt.Errorf("confirm-actions is on and the page tool %s could not be classified before it ran: %w", name, err)
	}
	var tool *snapshot.PageToolDescriptor
	for i := range listing.Tools {
		if listing.Tools[i].Name == name {
			tool = &listing.Tools[i]
			break
		}
	}
	if tool == nil {
		return nil
	}
	origin, err := s.currentPageOrigin(ctx, "")
	if err != nil {
		return err
	}
	return s.consent.CheckAction(siteconsent.ActionRequest{
		Tool:          "brw_call_page_tool " + name,
		Origin:        origin,
		Label:         strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(name) + " " + tool.Description),
		Consequential: tool.Consequential(),
	})
}

func (s *Server) pageToolTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	reporter, ok := s.manager.(browser.ActiveTabReporter)
	if !ok {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pageToolTabLookupTimeout)
	defer cancel()
	tabID, err := reporter.ActiveTabID(lookupCtx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(tabID)
}
