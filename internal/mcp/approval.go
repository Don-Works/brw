package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// SetApprovalGate installs the asynchronous operator approval boundary.
func (s *Server) SetApprovalGate(gate *approvalgate.Gate) {
	s.approvalGate = gate
	gate.SetConsentGuard(s.consent)
}

func (s *Server) checkApproval(ctx context.Context, name string, args json.RawMessage, consentErr error) (context.Context, json.RawMessage, error) {
	var required *siteconsent.ConfirmationRequiredError
	if consentErr != nil && (s.approvalGate == nil || !errors.As(consentErr, &required)) {
		return ctx, args, consentErr
	}
	if s.approvalGate == nil {
		var probe struct {
			ID string `json:"approval_id"`
		}
		if json.Unmarshal(args, &probe) == nil && probe.ID != "" {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(args, &fields)
			delete(fields, "approval_id")
			args, _ = json.Marshal(fields)
			ctx = approval.WithRequestID(ctx, probe.ID)
		}
		return ctx, args, nil
	}
	return s.approvalGate.Check(ctx, name, args, s.sessionID, func(ref string) string { return s.refLabels.label(browser.TabIDFromContext(ctx), ref) }, required != nil)
}

func (s *Server) approvalStatus(ctx context.Context, id string) (any, error) {
	if s.approvalGate != nil {
		return s.approvalGate.Status(id)
	}
	if remote, ok := s.manager.(interface {
		ApprovalStatus(context.Context, string) (map[string]any, error)
	}); ok {
		return remote.ApprovalStatus(ctx, id)
	}
	return nil, errors.New("approvals are not enabled")
}

func (s *Server) resumeApproval(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	if s.approvalGate == nil {
		if _, ok := s.manager.(interface {
			ApprovalStatus(context.Context, string) (map[string]any, error)
		}); !ok {
			return toolError(errors.New("approvals are not enabled")), nil
		}
	}
	var req struct {
		ID        string                     `json:"approval_id"`
		Tool      string                     `json:"tool"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := unmarshalStrictArgs(raw, &req); err != nil {
		return nil, invalid(err)
	}
	if req.ID == "" || req.Tool == "" || req.Arguments == nil || strings.HasPrefix(canonicalToolName(req.Tool), "brw_approval_") {
		return nil, invalid(errors.New("resume requires approval_id, original tool and original arguments"))
	}
	req.Arguments["approval_id"], _ = json.Marshal(req.ID)
	args, _ := json.Marshal(req.Arguments)
	return s.callTool(ctx, req.Tool, args)
}
