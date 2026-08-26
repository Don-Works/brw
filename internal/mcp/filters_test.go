package mcp

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func msg(level, text string) browser.ConsoleMessage {
	return browser.ConsoleMessage{Level: level, Text: text}
}

func texts(messages []browser.ConsoleMessage) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, m.Text)
	}
	return out
}

func TestConsoleBufferOnlyErrorsRetainsTheRest(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{
		msg("log", "boot"),
		msg("error", "boom"),
		msg("info", "ready"),
	})

	got, matched, truncated := buf.take(consoleQuery{OnlyErrors: true}, nil)
	if len(got) != 1 || got[0].Text != "boom" {
		t.Fatalf("only_errors returned %v, want [boom]", texts(got))
	}
	if matched != 1 {
		t.Fatalf("matched = %d, want 1", matched)
	}
	if truncated {
		t.Fatal("unbounded result reported as truncated")
	}

	// The backend drains its own buffer on every read, so anything a filter
	// skips is gone forever unless brw retains it. A later, wider read must
	// still see the log lines this one filtered out.
	rest, _, _ := buf.take(consoleQuery{}, nil)
	if want := []string{"boot", "ready"}; len(rest) != 2 || rest[0].Text != want[0] || rest[1].Text != want[1] {
		t.Fatalf("filtered-out messages were destroyed: got %v, want %v", texts(rest), want)
	}
}

func TestConsoleBufferPatternFilters(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{
		msg("error", "TypeError: x is not a function"),
		msg("error", "failed to fetch /api/orders"),
		msg("log", "render complete"),
	})

	got, _, _ := buf.take(consoleQuery{Pattern: "TypeError"}, regexp.MustCompile("TypeError"))
	if len(got) != 1 || got[0].Text != "TypeError: x is not a function" {
		t.Fatalf("pattern returned %v", texts(got))
	}
}

func TestConsoleBufferLimitTakesMostRecent(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{
		msg("log", "one"), msg("log", "two"), msg("log", "three"),
	})

	got, matched, truncated := buf.take(consoleQuery{Limit: 2}, nil)
	if want := []string{"two", "three"}; len(got) != 2 || got[0].Text != want[0] || got[1].Text != want[1] {
		t.Fatalf("limit returned %v, want the most recent %v", texts(got), want)
	}
	if matched != 3 {
		t.Fatalf("matched = %d, want 3 (the count before the limit)", matched)
	}
	if !truncated {
		t.Fatal("limit cut the result but truncated was not set")
	}
}

func TestConsoleBufferClearFalseKeepsMessagesReadable(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{msg("error", "boom")})

	first, _, _ := buf.take(consoleQuery{Clear: boolPtr(false)}, nil)
	second, _, _ := buf.take(consoleQuery{Clear: boolPtr(false)}, nil)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("clear:false lost the message: first=%v second=%v", texts(first), texts(second))
	}
}

func TestConsoleBufferDefaultsToDraining(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{msg("error", "boom")})

	if got, _, _ := buf.take(consoleQuery{}, nil); len(got) != 1 {
		t.Fatalf("first read returned %v, want the message", texts(got))
	}
	if got, _, _ := buf.take(consoleQuery{}, nil); len(got) != 0 {
		t.Fatalf("second read returned %v, want nothing after the default drain", texts(got))
	}
}

func TestConsoleBufferRetentionDropsOldest(t *testing.T) {
	var buf consoleBuffer
	for i := 0; i < consoleRetention+50; i++ {
		buf.ingest([]browser.ConsoleMessage{msg("log", "m")})
	}
	if len(buf.messages) != consoleRetention {
		t.Fatalf("buffer held %d messages, want the %d cap", len(buf.messages), consoleRetention)
	}
}

func TestConsoleLevelFilterIsExact(t *testing.T) {
	var buf consoleBuffer
	buf.ingest([]browser.ConsoleMessage{msg("warn", "deprecated"), msg("error", "boom")})

	got, _, _ := buf.take(consoleQuery{Level: "WARN"}, nil)
	if len(got) != 1 || got[0].Text != "deprecated" {
		t.Fatalf("level filter returned %v, want [deprecated] case-insensitively", texts(got))
	}
}

func TestCompileURLPatternRejectsBadRegex(t *testing.T) {
	if got, err := compileURLPattern(""); got != nil || err != nil {
		t.Fatalf("empty pattern = (%v, %v), want (nil, nil)", got, err)
	}
	if _, err := compileURLPattern("[unclosed"); err == nil {
		t.Fatal("invalid regex accepted; it would have silently matched nothing")
	}
}

func TestFilterNetworkRequestsPatternAndLimit(t *testing.T) {
	entries := []browser.NetworkRequest{
		{URL: "https://example.com/app.js"},
		{URL: "https://example.com/api/orders"},
		{URL: "https://example.com/api/users"},
		{URL: "https://cdn.example.com/logo.png"},
	}

	got := filterNetworkRequests(entries, regexp.MustCompile(`/api/`), 0)
	if got.Returned != 2 || got.Matched != 2 {
		t.Fatalf("pattern returned %d of %d, want 2 of 2", got.Returned, got.Matched)
	}
	if got.Truncated {
		t.Fatal("result under the limit reported as truncated")
	}

	limited := filterNetworkRequests(entries, nil, 1)
	if limited.Returned != 1 || limited.Matched != 4 || !limited.Truncated {
		t.Fatalf("limit gave returned=%d matched=%d truncated=%v, want 1/4/true",
			limited.Returned, limited.Matched, limited.Truncated)
	}
	if limited.Requests[0].URL != "https://cdn.example.com/logo.png" {
		t.Fatalf("limit kept %q, want the most recent request", limited.Requests[0].URL)
	}
}

func TestFilterCapturedRequestsSharesTheSameRules(t *testing.T) {
	entries := []snapshot.CapturedRequest{
		{URL: "https://example.com/api/a"},
		{URL: "https://example.com/static/b"},
	}
	got := filterCapturedRequests(entries, regexp.MustCompile(`/api/`), 0)
	if got.Returned != 1 || got.Requests[0].URL != "https://example.com/api/a" {
		t.Fatalf("captured filter returned %+v", got.Requests)
	}
}

func TestFilterNetworkEmptyResultMarshalsAsArray(t *testing.T) {
	got := filterNetworkRequests(nil, nil, 0)
	if got.Requests == nil {
		t.Fatal("empty result has a nil slice; it would marshal as null instead of []")
	}
}

func TestNormalizeRepeatBounds(t *testing.T) {
	if got, err := normalizeRepeat(0); got != 1 || err != nil {
		t.Fatalf("repeat 0 = (%d, %v), want (1, nil) so an omitted field acts as before", got, err)
	}
	if got, err := normalizeRepeat(maxRepeat); got != maxRepeat || err != nil {
		t.Fatalf("repeat %d = (%d, %v), want it accepted", maxRepeat, got, err)
	}
	for _, bad := range []int{-1, maxRepeat + 1} {
		if _, err := normalizeRepeat(bad); err == nil {
			t.Fatalf("repeat %d accepted, want rejected", bad)
		}
	}
}

func TestRepeatActionRunsExactlyNTimes(t *testing.T) {
	calls := 0
	got, err := repeatAction(context.Background(), 5, func(context.Context) (browser.ActionResult, error) {
		calls++
		return browser.ActionResult{URL: "final"}, nil
	})
	if err != nil {
		t.Fatalf("repeatAction: %v", err)
	}
	if calls != 5 {
		t.Fatalf("ran %d times, want 5", calls)
	}
	if got.URL != "final" {
		t.Fatalf("returned %+v, want the last result", got)
	}
}

func TestRepeatActionStopsOnFirstError(t *testing.T) {
	calls := 0
	boom := errors.New("boom")
	_, err := repeatAction(context.Background(), 10, func(context.Context) (browser.ActionResult, error) {
		calls++
		if calls == 3 {
			return browser.ActionResult{}, boom
		}
		return browser.ActionResult{}, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if calls != 3 {
		t.Fatalf("ran %d times, want 3 — a failing repeat must not keep going", calls)
	}
}

// A cancelled request must stop a long repeat rather than hold the tab for the
// full count.
func TestRepeatActionHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := repeatAction(ctx, 100, func(context.Context) (browser.ActionResult, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return browser.ActionResult{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 2 {
		t.Fatalf("ran %d times after cancellation, want 2", calls)
	}
}
