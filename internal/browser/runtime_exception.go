package browser

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatRuntimeException renders a CDP Runtime.ExceptionDetails payload as a
// short, readable message for tool output.
//
// The raw payload is marshalled bookkeeping: objectId, scriptId, exceptionId,
// subtype, and a full stackTrace object with a callFrames array. Emitting it
// verbatim spent several hundred characters of the agent's context on every
// failed brw_evaluate and buried the one line that matters — the thrown
// value's own description, which already embeds the JS stack.
//
// Prefer that description; fall back to the "Uncaught" text plus the source
// position when the thrown value carries no description (for example when a
// non-Error value is thrown). Returns "" when nothing useful can be extracted,
// so callers can keep their raw-payload fallback and lose no information.
func FormatRuntimeException(details any) string {
	if details == nil {
		return ""
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return ""
	}
	var parsed struct {
		Text      string `json:"text"`
		Line      int    `json:"lineNumber"`
		Column    int    `json:"columnNumber"`
		Exception struct {
			Description string `json:"description"`
		} `json:"exception"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if msg := strings.TrimSpace(parsed.Exception.Description); msg != "" {
		return msg
	}
	msg := strings.TrimSpace(parsed.Text)
	if msg == "" {
		return ""
	}
	// CDP reports zero-based positions; humans and JS stacks are one-based.
	return fmt.Sprintf("%s at line %d, column %d", msg, parsed.Line+1, parsed.Column+1)
}
