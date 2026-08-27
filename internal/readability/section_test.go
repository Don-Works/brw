package readability

import (
	"strings"
	"testing"
)

// offsetOf builds the heading offset the in-page extractor computes, as a
// pointer so an absent offset stays distinguishable from position zero.
func offsetOf(main, heading string) *int {
	at := strings.Index(main, heading)
	return &at
}

func intPtr(v int) *int { return &v }

// sectionedRead builds a document whose prose contains its headings verbatim,
// matching what the in-page extractor produces (both go through the same
// whitespace-collapsing text() helper).
func sectionedRead() PageRead {
	main := "Intro one two three " + // 0
		"Install Run the installer " + // 20
		"Linux apt install it " + // 47
		"macOS brew install it " + // 68
		"Usage Call the binary" // 91
	return PageRead{
		Main: main,
		Headings: []Heading{
			{Level: 1, Text: "Intro", Offset: offsetOf(main, "Intro")},
			{Level: 1, Text: "Install", Offset: offsetOf(main, "Install")},
			{Level: 2, Text: "Linux", Offset: offsetOf(main, "Linux")},
			{Level: 2, Text: "macOS", Offset: offsetOf(main, "macOS")},
			{Level: 1, Text: "Usage", Offset: offsetOf(main, "Usage")},
		},
	}
}

func TestFindSectionSpanEndsAtNextSiblingHeading(t *testing.T) {
	read := sectionedRead()
	span, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Install")
	if !ok {
		t.Fatal("Install not found")
	}
	got := string([]rune(read.Main)[span.Start:span.End])

	// A deeper heading belongs to the section; the next same-level heading ends it.
	if !strings.Contains(got, "Linux") || !strings.Contains(got, "macOS") {
		t.Fatalf("section dropped its subsections: %q", got)
	}
	if strings.Contains(got, "Usage") {
		t.Fatalf("section ran past the next sibling heading: %q", got)
	}
	if !strings.HasPrefix(got, "Install") {
		t.Fatalf("section did not start at its heading: %q", got)
	}
}

func TestFindSectionSpanSubsectionStopsAtSibling(t *testing.T) {
	read := sectionedRead()
	span, _ := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Linux")
	got := string([]rune(read.Main)[span.Start:span.End])

	if !strings.Contains(got, "apt install") {
		t.Fatalf("subsection missing its own content: %q", got)
	}
	if strings.Contains(got, "brew") {
		t.Fatalf("subsection ran into its sibling: %q", got)
	}
}

func TestFindSectionSpanLastSectionRunsToEnd(t *testing.T) {
	read := sectionedRead()
	span, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Usage")
	if !ok {
		t.Fatal("Usage not found")
	}
	if span.End != len([]rune(read.Main)) {
		t.Fatalf("last section ended at %d, want the end of the document (%d)", span.End, len([]rune(read.Main)))
	}
}

// An exact match must win over a substring one, or asking for a short heading
// silently returns a longer one that merely contains it.
func TestFindSectionSpanPrefersExactMatch(t *testing.T) {
	main := "Installation notes here Install do this"
	headings := []Heading{
		{Level: 1, Text: "Installation", Offset: offsetOf(main, "Installation")},
		{Level: 1, Text: "Install", Offset: offsetOf(main, "Install do")},
	}
	span, ok := FindSectionSpan(headings, len([]rune(main)), "Install")
	if !ok {
		t.Fatal("Install not found")
	}
	if span.Heading != "Install" {
		t.Fatalf("matched %q, want the exact heading Install", span.Heading)
	}
}

func TestFindSectionSpanIsCaseInsensitive(t *testing.T) {
	read := sectionedRead()
	if _, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "  iNsTaLl  "); !ok {
		t.Fatal("case-insensitive trimmed match failed")
	}
}

func TestFindSectionSpanSkipsUnaddressableHeadings(t *testing.T) {
	// Offset -1 marks a heading outside the extracted prose. Selecting it would
	// slice from a guessed position, so it must not be addressable at all.
	headings := []Heading{{Level: 1, Text: "Sidebar", Offset: intPtr(-1)}}
	if _, ok := FindSectionSpan(headings, 100, "Sidebar"); ok {
		t.Fatal("a heading with no offset was treated as addressable")
	}
	if names := SectionNames(headings); len(names) != 0 {
		t.Fatalf("SectionNames listed an unaddressable heading: %v", names)
	}
}

func TestFindSectionSpanMissesCleanly(t *testing.T) {
	read := sectionedRead()
	if _, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Nonexistent"); ok {
		t.Fatal("unknown section reported as found")
	}
	if _, ok := FindSectionSpan(nil, 100, "Anything"); ok {
		t.Fatal("section found in a document with no headings")
	}
	if _, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "   "); ok {
		t.Fatal("blank section name reported as found")
	}
}

func TestWindowSectionReturnsOnlyThatSpan(t *testing.T) {
	read := sectionedRead()
	got := Window(read, ReadOptions{Section: "Usage"})

	if got.Section != "Usage" || got.SectionLevel != 1 {
		t.Fatalf("section echo = %q/%d, want Usage/1", got.Section, got.SectionLevel)
	}
	if !strings.Contains(got.Main, "Call the binary") {
		t.Fatalf("section content missing: %q", got.Main)
	}
	if strings.Contains(got.Main, "apt install") {
		t.Fatalf("section leaked another section's content: %q", got.Main)
	}
	// Bounds apply within the section, so main_total_chars is the section's
	// length rather than the document's.
	if got.MainTotalChars >= len([]rune(read.Main)) {
		t.Fatalf("main_total_chars = %d, want the section length not the document length", got.MainTotalChars)
	}
}

// A section still pages, so a huge section is not a way around the bound.
func TestWindowSectionStillPages(t *testing.T) {
	main := "Big " + strings.Repeat("x", 100)
	read := PageRead{
		Main:     main,
		Headings: []Heading{{Level: 1, Text: "Big", Offset: intPtr(0)}},
	}
	got := Window(read, ReadOptions{Section: "Big", MaxChars: 30})
	if len([]rune(got.Main)) != 30 {
		t.Fatalf("section prose = %d chars, want the 30-char bound", len([]rune(got.Main)))
	}
	if !got.MainTruncated || got.NextOffset != 30 {
		t.Fatalf("section paging metadata wrong: truncated=%v next_offset=%d", got.MainTruncated, got.NextOffset)
	}
}

// A read from a source that does not compute heading offsets must not be
// silently treated as a document whose headings all start at position zero.
// Found by driving this build against an older upstream in proxy mode: every
// section resolved to offset 0 and returned the whole page while reporting that
// it had found the section.
func TestSectionsUnavailableWhenOffsetsAreAbsent(t *testing.T) {
	read := PageRead{
		Main: "Intro one two three Install Run the installer Usage Call the binary",
		Headings: []Heading{
			{Level: 1, Text: "Intro"},
			{Level: 1, Text: "Install"},
			{Level: 1, Text: "Usage"},
		},
	}

	if SectionsAddressable(read.Headings) {
		t.Fatal("headings with no offsets reported as addressable")
	}
	if _, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Usage"); ok {
		t.Fatal("a section resolved from headings that carry no offsets")
	}
	if names := SectionNames(read.Headings); len(names) != 0 {
		t.Fatalf("SectionNames offered %v from headings with no offsets", names)
	}

	// Window must leave the prose alone rather than slice a span it cannot
	// compute, and must not claim to have resolved a section.
	got := Window(read, ReadOptions{Section: "Usage", MaxChars: UnboundedReadChars})
	if got.Main != read.Main {
		t.Fatalf("prose was sliced despite unusable offsets: %q", got.Main)
	}
	if got.Section != "" {
		t.Fatalf("reported section %q from a read that cannot address sections", got.Section)
	}
}

// A heading genuinely at position zero is addressable; only a nil offset means
// unknown.
func TestSectionAtPositionZeroIsAddressable(t *testing.T) {
	read := PageRead{
		Main:     "Intro one two Usage three",
		Headings: []Heading{{Level: 1, Text: "Intro", Offset: intPtr(0)}},
	}
	if !SectionsAddressable(read.Headings) {
		t.Fatal("a heading at offset 0 was treated as having no offset")
	}
	span, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), "Intro")
	if !ok || span.Start != 0 {
		t.Fatalf("span = %+v, ok = %v, want a span starting at 0", span, ok)
	}
}
