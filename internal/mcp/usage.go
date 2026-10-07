package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/usagelog"
)

var usageToolsOnce sync.Once
var usageTools map[string]bool

func IsKnownToolName(name string) bool {
	usageToolsOnce.Do(func() {
		usageTools = make(map[string]bool)
		for _, name := range ToolNames() {
			usageTools[name] = true
		}
	})
	return usageTools[name]
}

func (s *Server) recordToolUsage(ctx context.Context, name string, args json.RawMessage, started time.Time, result any, rpcErr *rpcError) {
	operation := canonicalToolName(name)
	if !IsKnownToolName(operation) {
		operation = "unknown_tool"
	}
	s.recordMCPUsage(ctx, operation, "tool", "mcp_arguments_result", args, started, result, rpcErr)
}

func (s *Server) recordMCPUsage(ctx context.Context, operation, scope, representation string, input []byte, started time.Time, result any, rpcErr *rpcError) {
	reporter, forwarding := s.manager.(interface {
		ReportUsage(context.Context, usagelog.Event) error
	})
	if s.usage == nil && !forwarding {
		return
	}
	outcome, errorClass, fingerprint := mcpUsageOperationOutcome(operation, result, rpcErr)
	elapsed := time.Since(started)
	event := usagelog.Event{
		Layer: "mcp", Operation: operation, Outcome: outcome, Scope: scope, Representation: representation,
		DurationMS: elapsed.Milliseconds(), DurationUS: elapsed.Microseconds(),
		ErrorClass: errorClass, ErrorFingerprint: fingerprint,
		Retryable: usagelog.Retryable(errorClass), SessionID: s.sessionID, RequestID: usagelog.RequestID(ctx),
		InputBytes: usagelog.Count(int64(len(input))),
	}
	if observation := usagelog.ObservationFromContext(ctx); observation != nil {
		observation.Apply(&event)
	}
	chars, binary := usagelog.MeasureJSON(input)
	event.InputTextChars = usagelog.Count(chars)
	event.EstimatedInputTokensChars4 = usagelog.Count(usagelog.EstimateTokens(chars))
	event.BinaryInputBytes = usagelog.Count(binary)
	measured := result
	if rpcErr != nil {
		measured = rpcErr
	}
	if encoded, err := json.Marshal(measured); err == nil {
		event.OutputBytes = usagelog.Count(int64(len(encoded)))
		var obj map[string]any
		_ = json.Unmarshal(encoded, &obj)
		if scope == "catalogue" || rpcErr != nil {
			chars, binary = usagelog.MeasureJSON(encoded)
		} else {
			chars, binary = 0, 0
			content, _ := obj["content"].([]any)
			for _, item := range content {
				block, _ := item.(map[string]any)
				switch block["type"] {
				case "text":
					if text, ok := block["text"].(string); ok {
						c, b := usagelog.MeasureJSON([]byte(text))
						chars += c
						binary += b
					}
				case "image", "audio":
					if data, ok := block["data"].(string); ok {
						binary += int64(len(data))
					}
				case "resource":
					if resource, ok := block["resource"].(map[string]any); ok {
						if text, ok := resource["text"].(string); ok {
							chars += int64(utf8.RuneCountInString(text))
						}
					}
				}
			}
			if structured, ok := obj["structuredContent"]; ok {
				if data, err := json.Marshal(structured); err == nil {
					event.StructuredOutputBytes = usagelog.Count(int64(len(data)))
				}
			}
		}
		event.OutputTextChars = usagelog.Count(chars)
		event.EstimatedOutputTokensChars4 = usagelog.Count(usagelog.EstimateTokens(chars))
		event.BinaryOutputBytes = usagelog.Count(binary)
	}
	if s.usage != nil {
		_ = s.usage.Record(event)
		return
	}
	if err := reporter.ReportUsage(ctx, event); err != nil {
		log.Print("usage metadata report failed")
	}
}

func semanticToolFailure(operation string, result any) (failed, cancelled bool, message string) {
	payload, ok := result.(map[string]any)
	if !ok {
		return
	}
	switch operation {
	case "brw_batch":
		if batch, ok := payload["structuredContent"].(browser.BatchResult); ok {
			return !batch.OK, batch.Cancelled, batch.Error
		}
	case "brw_plan":
		if plan, ok := payload["structuredContent"].(browser.PlanResult); ok {
			return !plan.OK, plan.Cancelled, plan.Error
		}
	case "brw_find":
		if found, ok := payload["structuredContent"].(browser.FindActResult); ok && found.Action != "" {
			return !found.Result.OK, false, found.Result.Message
		}
	default:
		if !slices.Contains(observeToolNames(), operation) && operation != "brw_upload_file" {
			return
		}
		switch action := payload["structuredContent"].(type) {
		case browser.ActionResult:
			return !action.OK, false, action.Message
		case navigationWithSurfaces:
			if operation == "brw_navigate" || operation == "brw_navigate_to" {
				return !action.OK, false, action.Message
			}
		}
	}
	return
}

func withSemanticFailure(operation string, result any) any {
	if failed, cancelled, _ := semanticToolFailure(operation, result); failed && !cancelled {
		result.(map[string]any)["isError"] = true
	}
	return result
}

func mcpUsageOperationOutcome(operation string, result any, rpcErr *rpcError) (outcome, errorClass, fingerprint string) {
	if rpcErr == nil {
		if failed, cancelled, message := semanticToolFailure(operation, result); failed {
			message = cmp.Or(message, "action failed")
			errorClass = cmp.Or(usagelog.SafeErrorClass(usagelog.ClassifyError(errors.New(message))), "tool")
			if cancelled {
				errorClass = "canceled"
			}
			return "error", errorClass, usagelog.Fingerprint(message)
		}
	}
	return mcpUsageOutcome(result, rpcErr)
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
		if code, ok := structured["error"].(string); ok && usagelog.SafeErrorClass(code) != "" {
			errorClass = code
		}
	}
	message := "tool error"
	if content, ok := m["content"].([]toolContent); ok && len(content) > 0 {
		message = content[0].Text
	} else if content, ok := m["content"].([]any); ok && len(content) > 0 {
		message = fmt.Sprint(content[0])
	}
	if errorClass == "tool" {

		if inferred := usagelog.ClassifyError(errors.New(message)); inferred != "" {
			errorClass = inferred
		}
	}
	return "error", errorClass, usagelog.Fingerprint(message)
}

func usageRequestID(ctx context.Context) string {
	if id := usagelog.RequestID(ctx); id != "" {
		return id
	}
	return usagelog.NewID()
}

func setReadObservation(ctx context.Context, read readability.PageRead) {
	if observation := usagelog.ObservationFromContext(ctx); observation != nil {
		observation.ResultTruncated = usagelog.Flag(read.MainTruncated || read.LinksTruncated || read.HeadingsTruncated || read.TablesTruncated)
	}
}
