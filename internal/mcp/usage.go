package mcp

import (
	"fmt"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

func (s *Server) recordToolUsage(name string, started time.Time, result any, rpcErr *rpcError) {
	if s.usage == nil {
		return
	}
	operation := canonicalToolName(name)
	if usagelog.SafeID(operation) == "" {
		operation = "unknown_tool"
	}
	outcome, errorClass, fingerprint := mcpUsageOutcome(result, rpcErr)
	_ = s.usage.Record(usagelog.Event{
		Layer: "mcp", Operation: operation, Outcome: outcome,
		DurationMS: time.Since(started).Milliseconds(),
		ErrorClass: errorClass, ErrorFingerprint: fingerprint,
		Retryable: usagelog.Retryable(errorClass), SessionID: s.sessionID,
	})
}

func mcpUsageOutcome(result any, rpcErr *rpcError) (outcome, errorClass, fingerprint string) {
	if rpcErr != nil {
		return "error", "rpc", usagelog.Fingerprint(rpcErr.Message)
	}
	m, ok := result.(map[string]any)
	if !ok {
		return "ok", "", ""
	}
	isError, _ := m["isError"].(bool)
	if !isError {
		return "ok", "", ""
	}
	errorClass = "tool"
	if structured, ok := m["structuredContent"].(map[string]any); ok {
		if code, ok := structured["error"].(string); ok && usagelog.SafeID(code) != "" {
			errorClass = code
		}
	}
	message := "tool error"
	if content, ok := m["content"].([]toolContent); ok && len(content) > 0 {
		message = content[0].Text
	} else if content, ok := m["content"].([]any); ok && len(content) > 0 {
		message = fmt.Sprint(content[0])
	}
	return "error", errorClass, usagelog.Fingerprint(message)
}
