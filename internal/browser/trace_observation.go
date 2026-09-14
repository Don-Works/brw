package browser

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/snapshot"
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

// generatedScriptVerbs are the actions whose page script brw writes itself, and
// so the only labels the HTTP surface accepts from a caller. Letting a request
// name any action would let hand-written JavaScript record itself in the trace
// as a typed read, which is the distinction the label exists to draw.
var generatedScriptVerbs = map[string]bool{
	TraceActionGet:   true,
	TraceActionFrame: true,
}

// IsGeneratedScriptVerb reports whether action may be carried across the HTTP
// surface as an evaluation's trace label.
func IsGeneratedScriptVerb(action string) bool {
	return generatedScriptVerbs[strings.TrimSpace(action)]
}

// isGeneratedReadExpression reports whether expression is one of brw's own read
// scripts rather than a caller's JavaScript.
//
// The label alone cannot answer this. It crosses the HTTP surface as a request
// field, so an --upstream-http client — or anything else posting to
// /api/page/evaluate — can name arbitrary JavaScript a typed read. What cannot
// be forged is the script: brw builds both generated reads as one constant
// applied to JSON-encoded string arguments and nothing else, so the whole
// expression is reproducible from the arguments it carries.
//
// The comparison is therefore against a REBUILT expression rather than a prefix.
// A prefix constrains only the start of the string and says nothing about what
// follows the generated call's closing paren, so
// BuildGetExpression(...) + ";document.getElementById('go').click()" would pass
// as a read and drive the page through a human's hold. Rebuilding accepts
// exactly the set brw itself emits, with the caller controlling only the string
// arguments to it.
func isGeneratedReadExpression(action, expression string) bool {
	switch strings.TrimSpace(action) {
	case TraceActionGet:
		args, ok := generatedScriptArgs(expression, snapshot.GetScript, 3)
		return ok && expression == snapshot.BuildGetExpression(args[0], args[1], args[2])
	case TraceActionFrame:
		args, ok := generatedScriptArgs(expression, snapshot.FrameSwitchScript, 1)
		return ok && expression == snapshot.BuildFrameSwitchExpression(args[0])
	default:
		return false
	}
}

// generatedScriptArgs recovers the arguments of `script(...)` when expression is
// that call and nothing else. Recovery is deliberately strict but not the
// security boundary: the caller rebuilds the call from what comes back and
// compares, so this only has to produce the arguments a genuine generated
// expression would have been built from.
func generatedScriptArgs(expression, script string, want int) ([]string, bool) {
	open := script + "("
	if !strings.HasPrefix(expression, open) || !strings.HasSuffix(expression, ")") {
		return nil, false
	}
	// Decoding the argument list as a JSON array is what rejects trailing code:
	// encoding/json refuses anything after the closing bracket, so the arguments
	// of `script("a","b","c");evil()` do not parse as one.
	var args []string
	if err := json.Unmarshal([]byte("["+expression[len(open):len(expression)-1]+"]"), &args); err != nil {
		return nil, false
	}
	if len(args) != want {
		return nil, false
	}
	return args, true
}

// traceLabelAction is the semantic verb an evaluation was made under, or
// "evaluate" when the caller set none.
func traceLabelAction(ctx context.Context) string {
	if label, ok := TraceLabelFromCtx(ctx); ok {
		return label.Action
	}
	return TraceActionEvaluate
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
