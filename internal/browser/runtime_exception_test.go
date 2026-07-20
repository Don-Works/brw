package browser

import (
	"encoding/json"
	"strings"
	"testing"
)

// realCDPExceptionDetails is the payload Chrome actually returned for
// `throw new Error('boom')` on the extension bridge. Marshalled verbatim it
// was ~380 characters of CDP bookkeeping in the agent's context.
const realCDPExceptionDetails = `{
  "columnNumber": 0,
  "exception": {
    "className": "Error",
    "description": "Error: boom\n    at <anonymous>:1:7",
    "objectId": "6734717020713938081.2.2",
    "subtype": "error",
    "type": "object"
  },
  "exceptionId": 1,
  "lineNumber": 0,
  "scriptId": "27",
  "stackTrace": {
    "callFrames": [
      {"columnNumber": 6, "functionName": "", "lineNumber": 0, "scriptId": "27", "url": ""}
    ]
  },
  "text": "Uncaught"
}`

func TestFormatRuntimeExceptionPrefersDescription(t *testing.T) {
	var details any
	if err := json.Unmarshal([]byte(realCDPExceptionDetails), &details); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := FormatRuntimeException(details)
	want := "Error: boom\n    at <anonymous>:1:7"
	if got != want {
		t.Fatalf("FormatRuntimeException = %q, want %q", got, want)
	}

	// The point of the change: CDP bookkeeping must not reach the agent.
	for _, leaked := range []string{"objectId", "scriptId", "exceptionId", "callFrames", "subtype"} {
		if strings.Contains(got, leaked) {
			t.Errorf("formatted message leaks CDP field %q: %s", leaked, got)
		}
	}
	if raw, _ := json.Marshal(details); len(got) >= len(raw) {
		t.Errorf("formatted message (%d bytes) is not shorter than raw payload (%d bytes)", len(got), len(raw))
	}
}

func TestFormatRuntimeExceptionFallsBackToText(t *testing.T) {
	// A thrown non-Error value carries no exception.description; the "Uncaught"
	// text plus a one-based source position is all we can offer.
	var details any
	if err := json.Unmarshal([]byte(`{"text":"Uncaught","lineNumber":4,"columnNumber":11}`), &details); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := FormatRuntimeException(details)
	want := "Uncaught at line 5, column 12"
	if got != want {
		t.Fatalf("FormatRuntimeException = %q, want %q", got, want)
	}
}

func TestFormatRuntimeExceptionEmptyWhenUnusable(t *testing.T) {
	// Callers keep a raw-payload fallback, so "nothing useful" must be "" and
	// never a misleading partial message.
	for name, input := range map[string]any{
		"nil":           nil,
		"empty object":  map[string]any{},
		"blank text":    map[string]any{"text": "   "},
		"unmarshalable": make(chan int),
		"wrong shape":   []string{"not", "an", "object"},
		"blank desc":    map[string]any{"exception": map[string]any{"description": "  "}},
	} {
		if got := FormatRuntimeException(input); got != "" {
			t.Errorf("%s: FormatRuntimeException = %q, want empty", name, got)
		}
	}
}
