package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

type approvalRequiredKey struct{}

// SetApprovalGate installs the shared asynchronous execution gate.
func (s *Server) SetApprovalGate(gate *approvalgate.Gate) {
	s.approvalGate = gate
	gate.SetConsentGuard(s.consent)
}

func (s *Server) approvalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.approvalGate == nil {
			next.ServeHTTP(w, r)
			return
		}
		operation := usageOperations[r.URL.Path]
		if operation == "" || approvalgate.ReadOnly(operation) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := readConsentBody(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		if r.Method == http.MethodGet {
			args := map[string]any{}
			for key, values := range r.URL.Query() {
				if len(values) == 1 {
					args[key] = values[0]
				}
			}
			body, _ = json.Marshal(args)
		}
		if id := r.Header.Get("X-Brw-Approval-Id"); id != "" {
			var fields map[string]json.RawMessage
			if json.Unmarshal(body, &fields) != nil || fields == nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "approval retry requires JSON arguments"})
				return
			}
			if _, duplicate := fields["approval_id"]; duplicate {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "supply approval_id once"})
				return
			}
			fields["approval_id"], _ = json.Marshal(id)
			body, _ = json.Marshal(fields)
		}
		ctx := r.Context()
		if browser.TabIDFromContext(ctx) == "" {
			var probe struct {
				TabID string `json:"tab_id"`
			}
			_ = json.Unmarshal(body, &probe)
			if probe.TabID != "" {
				ctx = browser.WithTabID(ctx, probe.TabID)
			}
		}
		required, _ := ctx.Value(approvalRequiredKey{}).(bool)
		session := r.Header.Get(usagelog.HeaderSessionID)
		if owner := requestLeaseOwner(r); owner != "" {
			session = owner + ":" + session
		}
		ctx, clean, err := s.approvalGate.Check(ctx, operation, body, session, nil, required)
		if err != nil {
			writeError(w, err)
			return
		}
		if r.Method != http.MethodGet {
			r.Body = io.NopCloser(bytes.NewReader(clean))
			r.ContentLength = int64(len(clean))
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func markApprovalRequired(ctx context.Context) context.Context {
	return context.WithValue(ctx, approvalRequiredKey{}, true)
}
