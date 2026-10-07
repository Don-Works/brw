package readability

import (
	"strings"
	"testing"
)

func offsetOf(main, heading string) *int {
	at := strings.Index(main, heading)
	return &at
}

func intPtr(v int) *int { return &v }

func sectionedRead() PageRead {
	main := "Intro one two three " +
		"Install Run the installer " +
		"Linux apt install it " +
		"macOS brew install it " +
		"Usage Call the binary"
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

	if got.MainTotalChars >= len([]rune(read.Main)) {
		t.Fatalf("main_total_chars = %d, want the section length not the document length", got.MainTotalChars)
	}
}

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

	got := Window(read, ReadOptions{Section: "Usage", MaxChars: UnboundedReadChars})
	if got.Main != read.Main {
		t.Fatalf("prose was sliced despite unusable offsets: %q", got.Main)
	}
	if got.Section != "" {
		t.Fatalf("reported section %q from a read that cannot address sections", got.Section)
	}
}

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
