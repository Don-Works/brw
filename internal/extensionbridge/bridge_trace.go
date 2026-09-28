package extensionbridge

import (
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func (b *Bridge) finishObservedTrace(before bridgeActionBaseline, message string, result *browser.ActionResult) {
	if result == nil {
		return
	}
	if before.Started.IsZero() {
		before.Started = time.Now()
	}
	result.DurationMS = time.Since(before.Started).Milliseconds()
	entry := bridgeTraceEntry(message)
	// Structured operands from the call site win over anything parsed out of the
	// display message; the parse remains the fallback for actions supplying none.
	//
	// Taken as a whole struct rather than field by field. The field list this
	// replaced silently dropped every TraceEntry field added after it was
	// written, which is how the credential-sourced mark reached the direct-CDP
	// trace and not this one. Everything the outcome owns is assigned below.
	if before.Trace.Action != "" {
		entry = before.Trace
	}
	entry.TabID = result.TabID
	entry.OK = result.OK
	entry.DurationMS = result.DurationMS
	entry.Timestamp = time.Now().Format(time.RFC3339Nano)
	if result.OK {
		entry.Error = result.Warning
	} else {
		entry.Error = result.Message
	}
	b.appendTrace(entry)
}

func (b *Bridge) appendTrace(entry browser.TraceEntry) {
	b.traceMu.Lock()
	b.trace = append(b.trace, entry)
	if len(b.trace) > 500 {
		b.trace = b.trace[len(b.trace)-500:]
	}
	b.traceMu.Unlock()
}

// recordObservation records a navigation or read. It mirrors the direct-CDP
// Manager method of the same name, including its rule: a trace entry with no
// tab id is unscoped and visible to every caller of the shared daemon, so an
// observation that cannot name its tab is dropped rather than broadcast.
func (b *Bridge) recordObservation(tabID, action, text string, start time.Time, err error) {
	if strings.TrimSpace(tabID) == "" {
		return
	}
	entry := browser.NewObservationTrace(action, text, start, err)
	entry.TabID = tabID
	b.appendTrace(entry)
}

func bridgeTraceEntry(message string) browser.TraceEntry {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	entry := browser.TraceEntry{Action: "action", Text: message}
	setRef := func(action, value string) {
		entry.Action = action
		if fields := strings.Fields(strings.TrimSpace(value)); len(fields) > 0 {
			entry.Ref = fields[0]
			entry.Text = ""
		}
	}
	switch {
	case strings.HasPrefix(lower, "clicked text "):
		entry.Action = "click_text"
		entry.Text = strings.TrimSpace(message[len("clicked text "):])
	case strings.HasPrefix(lower, "clicked "):
		setRef("click", message[len("clicked "):])
	case strings.HasPrefix(lower, "hovered "):
		setRef("hover", message[len("hovered "):])
	case strings.HasPrefix(lower, "typed into "):
		setRef("type", message[len("typed into "):])
	case strings.HasPrefix(lower, "filled "):
		setRef("fill", message[len("filled "):])
	case strings.HasPrefix(lower, "selected "):
		setRef("select", message[len("selected "):])
	case strings.HasPrefix(lower, "pressed "):
		entry.Action = "press"
		entry.Text = strings.TrimSpace(message[len("pressed "):])
	case strings.HasPrefix(lower, "scrolled "):
		entry.Action = "scroll"
		entry.Text = strings.TrimSpace(message[len("scrolled "):])
	case strings.HasPrefix(lower, "navigated to "):
		entry.Action = "navigate_to"
		entry.Text = strings.TrimSpace(message[len("navigated to "):])
	case strings.HasPrefix(lower, "navigated "):
		entry.Action = "navigate"
		entry.Text = strings.TrimSpace(message[len("navigated "):])
	case strings.HasPrefix(lower, "uploaded "):
		entry.Action = "upload_file"
	case strings.HasPrefix(lower, "dragged "):
		entry.Action = "drag"
	case strings.HasPrefix(lower, "mouse_down "):
		entry.Action = "mouse_down"
	case strings.HasPrefix(lower, "mouse_up "):
		entry.Action = "mouse_up"
	case strings.Contains(lower, "-clicked"):
		entry.Action = "click_button"
	}
	return entry
}

func (b *Bridge) GetTrace() browser.TraceResult {
	b.traceMu.Lock()
	entries := append([]browser.TraceEntry(nil), b.trace...)
	b.traceMu.Unlock()
	return browser.TraceResult{Entries: entries, Count: len(entries)}
}

func (b *Bridge) ClearTrace() {
	b.traceMu.Lock()
	b.trace = b.trace[:0]
	b.traceMu.Unlock()
}
