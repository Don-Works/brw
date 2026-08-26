package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// consoleRetention bounds the server-side console buffer. Console output is the
// most verbose thing a page produces, and an agent that only wants errors used
// to have to pull every log line into its context to find them.
const consoleRetention = 1000

// defaultConsoleLimit caps an unfiltered console read. The most recent messages
// are the ones that explain what just happened, so the window is taken from the
// tail.
const defaultConsoleLimit = 100

// consoleQuery is the filter an agent applies to a console read.
type consoleQuery struct {
	OnlyErrors bool   `json:"only_errors"`
	Level      string `json:"level"`
	Pattern    string `json:"pattern"`
	Limit      int    `json:"limit"`
	// Clear defaults to true (the historical drain-on-read behaviour). Pointer
	// so an omitted field is distinguishable from an explicit false.
	Clear *bool `json:"clear"`
}

func (q consoleQuery) clears() bool {
	return q.Clear == nil || *q.Clear
}

// consoleBuffer retains messages drained from the browser so that filtering is
// non-destructive. The backend drains its own buffer on every read, so without
// this a `pattern` that matched nothing would discard the unmatched messages
// permanently — the filter would eat the very logs it was meant to search.
type consoleBuffer struct {
	mu       sync.Mutex
	messages []browser.ConsoleMessage
}

// ingest appends newly drained messages, trimming the oldest past the retention
// cap.
func (b *consoleBuffer) ingest(fresh []browser.ConsoleMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, fresh...)
	if overflow := len(b.messages) - consoleRetention; overflow > 0 {
		b.messages = append([]browser.ConsoleMessage(nil), b.messages[overflow:]...)
	}
}

// take returns the messages matching q, newest-window first, and removes only
// those returned when q clears. Messages that did not match are retained.
// truncated reports that the limit cut the match set, so an agent knows to read
// again rather than assume it has seen everything that matched.
func (b *consoleBuffer) take(q consoleQuery, match *regexp.Regexp) (messages []browser.ConsoleMessage, matched int, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	selected := make([]int, 0, len(b.messages))
	for i, msg := range b.messages {
		if !consoleMatches(msg, q, match) {
			continue
		}
		selected = append(selected, i)
	}

	matched = len(selected)
	limit := q.Limit
	if limit == 0 {
		limit = defaultConsoleLimit
	}
	if limit > 0 && len(selected) > limit {
		selected = selected[len(selected)-limit:]
		truncated = true
	}

	out := make([]browser.ConsoleMessage, 0, len(selected))
	returned := make(map[int]bool, len(selected))
	for _, i := range selected {
		out = append(out, b.messages[i])
		returned[i] = true
	}

	if q.clears() && len(returned) > 0 {
		kept := b.messages[:0]
		for i, msg := range b.messages {
			if !returned[i] {
				kept = append(kept, msg)
			}
		}
		b.messages = append([]browser.ConsoleMessage(nil), kept...)
	}
	return out, matched, truncated
}

func consoleMatches(msg browser.ConsoleMessage, q consoleQuery, match *regexp.Regexp) bool {
	if q.OnlyErrors && !isErrorLevel(msg.Level) {
		return false
	}
	if q.Level != "" && !strings.EqualFold(q.Level, msg.Level) {
		return false
	}
	if match != nil && !match.MatchString(msg.Text) {
		return false
	}
	return true
}

// isErrorLevel treats both error and warning-severity levels as errors, matching
// what an agent means by "only show me what went wrong".
func isErrorLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "assert", "exception", "severe":
		return true
	default:
		return false
	}
}

// consoleResult is the shape brw_console returns. The counts let an agent see
// that a filter hid something without paying for the hidden messages.
type consoleResult struct {
	Messages []browser.ConsoleMessage `json:"messages"`
	Returned int                      `json:"returned"`
	// Matched is how many buffered messages passed the filter, before the limit.
	Matched int `json:"matched"`
	// Retained is how many messages remain buffered — the ones a filter held
	// back, still readable by a later call with a wider filter.
	Retained  int  `json:"retained"`
	Truncated bool `json:"truncated,omitempty"`
}

// compileURLPattern turns an optional regular expression into a matcher,
// reporting a bad expression as an argument error rather than matching nothing.
func compileURLPattern(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	return compiled, nil
}

// defaultNetworkLimit bounds a network read. A busy page issues hundreds of
// requests; the recent ones are the ones tied to what the agent just did.
const defaultNetworkLimit = 100

type networkResult[T any] struct {
	Requests  []T  `json:"requests"`
	Returned  int  `json:"returned"`
	Matched   int  `json:"matched"`
	Truncated bool `json:"truncated,omitempty"`
}

func filterNetworkRequests(entries []browser.NetworkRequest, match *regexp.Regexp, limit int) networkResult[browser.NetworkRequest] {
	return applyNetworkFilter(entries, limit, func(entry browser.NetworkRequest) bool {
		return match == nil || match.MatchString(entry.URL)
	})
}

func filterCapturedRequests(entries []snapshot.CapturedRequest, match *regexp.Regexp, limit int) networkResult[snapshot.CapturedRequest] {
	return applyNetworkFilter(entries, limit, func(entry snapshot.CapturedRequest) bool {
		return match == nil || match.MatchString(entry.URL)
	})
}

func applyNetworkFilter[T any](entries []T, limit int, keep func(T) bool) networkResult[T] {
	matched := make([]T, 0, len(entries))
	for _, entry := range entries {
		if keep(entry) {
			matched = append(matched, entry)
		}
	}
	if limit == 0 {
		limit = defaultNetworkLimit
	}
	truncated := false
	out := matched
	if limit > 0 && len(matched) > limit {
		out = matched[len(matched)-limit:]
		truncated = true
	}
	if out == nil {
		out = []T{}
	}
	return networkResult[T]{Requests: out, Returned: len(out), Matched: len(matched), Truncated: truncated}
}

// maxRepeat bounds a repeated action. Matches the ceiling Claude in Chrome's
// computer tool uses, and keeps a runaway repeat from holding a tab hostage.
const maxRepeat = 100

// normalizeRepeat validates a repeat count. Zero means once, so an omitted
// field behaves exactly as it did before repeat existed.
func normalizeRepeat(repeat int) (int, error) {
	if repeat == 0 {
		return 1, nil
	}
	if repeat < 1 || repeat > maxRepeat {
		return 0, fmt.Errorf("repeat must be between 1 and %d, got %d", maxRepeat, repeat)
	}
	return repeat, nil
}

// repeatAction runs act n times and returns the last result. Repeating in the
// daemon collapses n model round trips into one: "press ArrowDown 20 times" used
// to cost 20 tool calls and 20 post-action observations.
func repeatAction(ctx context.Context, n int, act func(context.Context) (browser.ActionResult, error)) (browser.ActionResult, error) {
	var result browser.ActionResult
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var err error
		result, err = act(ctx)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}
