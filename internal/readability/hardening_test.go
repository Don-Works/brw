package readability

import (
	"strings"
	"testing"
)

// A caller-supplied bound near MaxInt overflowed offset+limit to a negative
// number and panicked the slice that followed — a single tool call could take
// the daemon down.
func TestWindowSurvivesExtremeBounds(t *testing.T) {
	read := PageRead{Main: "hello world this is prose"}
	total := len([]rune(read.Main))

	for _, opts := range []ReadOptions{
		{Offset: 1, MaxChars: 1<<63 - 1},
		{Offset: 1<<63 - 1, MaxChars: 10},
		{Offset: 1<<63 - 1, MaxChars: 1<<63 - 1},
		{Offset: total - 1, MaxChars: 1<<63 - 1},
		{MaxChars: 1<<62 + 1},
	} {
		got := Window(read, opts)
		if len([]rune(got.Main)) > total {
			t.Fatalf("opts %+v returned more prose than exists", opts)
		}
	}
}

// A huge bound must simply mean "all of it", not a truncation or an error.
func TestWindowExtremeBoundReturnsWholeDocument(t *testing.T) {
	read := PageRead{Main: "hello world"}
	got := Window(read, ReadOptions{MaxChars: 1<<63 - 1})
	if got.Main != read.Main {
		t.Fatalf("main = %q, want the whole document", got.Main)
	}
	if got.MainTruncated {
		t.Fatal("a bound larger than the document reported truncation")
	}
}

// FindSectionSpan must pick the NEAREST following sibling heading, not the
// first one that happens to appear in the slice.
func TestFindSectionSpanUsesNearestFollowingHeading(t *testing.T) {
	main := strings.Repeat("x", 200)
	// Deliberately out of document order.
	headings := []Heading{
		{Level: 1, Text: "A", Offset: intPtr(0)},
		{Level: 1, Text: "C", Offset: intPtr(100)},
		{Level: 1, Text: "B", Offset: intPtr(50)},
	}

	spanA, ok := FindSectionSpan(headings, len([]rune(main)), "A")
	if !ok {
		t.Fatal("A not found")
	}
	if spanA.End != 50 {
		t.Fatalf("A ends at %d, want 50 — the nearest following sibling, not the first in slice order", spanA.End)
	}

	spanB, _ := FindSectionSpan(headings, len([]rune(main)), "B")
	if spanB.End != 100 {
		t.Fatalf("B ends at %d, want 100", spanB.End)
	}

	spanC, _ := FindSectionSpan(headings, len([]rune(main)), "C")
	if spanC.End != 200 {
		t.Fatalf("C ends at %d, want the end of the document", spanC.End)
	}
}
