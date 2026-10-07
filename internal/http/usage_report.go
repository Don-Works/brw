package httpapi

import (
	"net/http"

	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/usagelog"
)

func (s *Server) reportUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var event usagelog.Event
	if err := decodeBody(w, r, &event, 8192, true); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !validUsageReport(event) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	clean := usagelog.Event{
		Layer: event.Layer, Operation: event.Operation, Outcome: event.Outcome, Scope: event.Scope, Representation: event.Representation,
		DurationMS: event.DurationMS, DurationUS: event.DurationUS,
		SnapshotMode: event.SnapshotMode, OutputFormat: event.OutputFormat, DeltaRequested: event.DeltaRequested, DeltaReturned: event.DeltaReturned, ResultTruncated: event.ResultTruncated, ReadSettleMS: event.ReadSettleMS, ElementLimit: event.ElementLimit, ReturnedElements: event.ReturnedElements,
		InputBytes: event.InputBytes, OutputBytes: event.OutputBytes, InputTextChars: event.InputTextChars, OutputTextChars: event.OutputTextChars,
		EstimatedInputTokensChars4: event.EstimatedInputTokensChars4, EstimatedOutputTokensChars4: event.EstimatedOutputTokensChars4,
		BinaryInputBytes: event.BinaryInputBytes, BinaryOutputBytes: event.BinaryOutputBytes, StructuredOutputBytes: event.StructuredOutputBytes,
		SessionID: r.Header.Get(usagelog.HeaderSessionID), RequestID: r.Header.Get(usagelog.HeaderRequestID), Client: "brw-usage-report",
	}
	if event.Outcome == "error" {
		clean.ErrorClass = usagelog.SafeErrorClass(event.ErrorClass)
		if clean.ErrorClass == "" {
			clean.ErrorClass = "tool"
		}
		clean.ErrorFingerprint = usagelog.SafeFingerprint(event.ErrorFingerprint)
		clean.Retryable = usagelog.Retryable(clean.ErrorClass)
	}
	if err := s.usage.Record(clean); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validUsageReport(event usagelog.Event) bool {
	if !usagelog.ValidObservation(event) {
		return false
	}
	if event.Outcome != "ok" && event.Outcome != "error" {
		return false
	}
	if !mcp.IsKnownToolName(event.Operation) && event.Operation != "unknown_tool" && event.Operation != "tools_list" {
		return false
	}
	switch event.Layer {
	case "mcp":
		if event.Operation == "tools_list" {
			if event.Scope != "catalogue" || event.Representation != "mcp_catalogue" {
				return false
			}
		} else if event.Scope != "tool" || event.Representation != "mcp_arguments_result" {
			return false
		}
	case "cli":
		if event.Scope != "projection" || event.Representation != "cli_stdout" {
			return false
		}
	default:
		return false
	}
	if event.DurationMS < 0 || event.DurationUS < 0 || event.DurationMS > 86400000 || event.DurationUS > 86400000000 {
		return false
	}
	for _, count := range []*int64{event.InputBytes, event.OutputBytes, event.InputTextChars, event.OutputTextChars, event.EstimatedInputTokensChars4, event.EstimatedOutputTokensChars4, event.BinaryInputBytes, event.BinaryOutputBytes, event.StructuredOutputBytes} {
		if count != nil && (*count < 0 || *count > 1<<40) {
			return false
		}
	}
	return true
}
