package browser

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Observation actions record how a session reached a page and what it read
// there, as opposed to the input actions (click, type, fill, press, ...) that
// change the page. Both belong in the trace: a session that opens a URL and
// reads it performs no input action at all, so without these its entire visit
// is invisible to anyone watching the daemon's activity — which is exactly the
// case an agent browsing on someone's behalf produces.
const (
	TraceActionOpen     = "open"
	TraceActionFocusTab = "focus_tab"
	TraceActionCloseTab = "close_tab"
	TraceActionRead     = "read"
	TraceActionReadData = "read_data"
	TraceActionEvaluate = "evaluate"
	TraceActionGet      = "get"
	TraceActionFrame    = "frame"
)

var observationActions = map[string]bool{
	TraceActionOpen:     true,
	TraceActionFocusTab: true,
	TraceActionCloseTab: true,
	TraceActionRead:     true,
	TraceActionReadData: true,
	TraceActionEvaluate: true,
	TraceActionGet:      true,
	TraceActionFrame:    true,
}

// IsObservationAction reports whether a traced action observed the page rather
// than acting on it. Replay uses it to explain such entries as "nothing to
// replay" instead of listing each one back as an unknown action.
func IsObservationAction(action string) bool {
	return observationActions[strings.TrimSpace(action)]
}

// maxTraceTextBytes bounds an observation's text. A URL is comfortably under
// it; an evaluate expression is frequently not, and the trace is a 500-entry
// ring buffer served over the HTTP control plane, so the whole script does not
// belong in it. The prefix still identifies what ran.
const maxTraceTextBytes = 512

// boundedTraceText truncates on a rune boundary and marks that it did, so a
// reader never mistakes a cut-off value for the whole one.
func boundedTraceText(text string) string {
	if len(text) <= maxTraceTextBytes {
		return text
	}
	cut := maxTraceTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// NewObservationTrace builds the entry for one observation. The caller supplies
// the tab, because an entry with no tab id is visible to every caller of a
// shared daemon and must never carry a URL — see recordObservation on each
// controller for the guard that enforces it.
func NewObservationTrace(action, text string, start time.Time, err error) TraceEntry {
	entry := TraceEntry{
		Action:     action,
		Text:       boundedTraceText(strings.TrimSpace(text)),
		OK:         err == nil,
		DurationMS: time.Since(start).Milliseconds(),
		Timestamp:  time.Now().Format(time.RFC3339Nano),
	}
	if err != nil {
		entry.Error = boundedTraceText(err.Error())
	}
	return entry
}
