package readability

import (
	"strings"
	"testing"
)

func sampleRead() PageRead {
	return PageRead{
		URL:      "https://example.com/article",
		Title:    "Article",
		Main:     strings.Repeat("a", 100),
		Headings: []Heading{{Level: 1, Text: "One"}, {Level: 2, Text: "Two"}},
		Links:    []Link{{Text: "first", Href: "/1"}, {Text: "second", Href: "/2"}},
		Forms:    []Form{{Name: "search"}},
		Tables:   []Table{{Caption: "prices"}},
		Metadata: Metadata{Lang: "en"},
	}
}

func TestWindowDefaultsLeaveShortReadsIntact(t *testing.T) {
	read := sampleRead()
	got := Window(read, ReadOptions{})

	if got.Main != read.Main {
		t.Fatalf("short prose was altered: got %d chars, want %d", len(got.Main), len(read.Main))
	}
	if got.MainTruncated {
		t.Fatal("short prose reported as truncated")
	}
	if got.NextOffset != 0 {
		t.Fatalf("next_offset = %d, want 0 when nothing was cut", got.NextOffset)
	}
	if len(got.Headings) != 2 || len(got.Links) != 2 || len(got.Forms) != 1 || len(got.Tables) != 1 {
		t.Fatalf("default window dropped sections: %+v", got)
	}
}

func TestWindowBoundsProseAndReportsNextOffset(t *testing.T) {
	read := sampleRead()
	got := Window(read, ReadOptions{MaxChars: 30})

	if len(got.Main) != 30 {
		t.Fatalf("prose = %d chars, want 30", len(got.Main))
	}
	if !got.MainTruncated {
		t.Fatal("truncated prose not flagged")
	}
	if got.MainTotalChars != 100 {
		t.Fatalf("main_total_chars = %d, want 100", got.MainTotalChars)
	}
	if got.NextOffset != 30 {
		t.Fatalf("next_offset = %d, want 30", got.NextOffset)
	}
}

// Paging must reassemble the original document exactly, or an agent that pages
// through a long article silently loses or duplicates a slice of it.
func TestWindowPagingReassemblesWholeDocument(t *testing.T) {
	read := sampleRead()
	read.Main = strings.Repeat("abcde", 40) // 200 chars

	var rebuilt strings.Builder
	offset := 0
	for i := 0; i < 10; i++ {
		page := Window(read, ReadOptions{MaxChars: 45, Offset: offset})
		rebuilt.WriteString(page.Main)
		if !page.MainTruncated {
			break
		}
		if page.NextOffset <= offset {
			t.Fatalf("next_offset did not advance: %d -> %d", offset, page.NextOffset)
		}
		offset = page.NextOffset
	}
	if rebuilt.String() != read.Main {
		t.Fatalf("paged reassembly mismatch:\n got %q\nwant %q", rebuilt.String(), read.Main)
	}
}

// Slicing prose by byte would split a multi-byte character and hand the agent
// invalid UTF-8 at every page boundary.
func TestWindowCutsOnRuneBoundaries(t *testing.T) {
	read := sampleRead()
	read.Main = strings.Repeat("日本語", 20) // 60 runes, 180 bytes

	page := Window(read, ReadOptions{MaxChars: 10})
	if got := []rune(page.Main); len(got) != 10 {
		t.Fatalf("prose = %d runes, want 10", len(got))
	}
	if strings.ContainsRune(page.Main, '�') {
		t.Fatalf("prose contains a replacement character, so a rune was split: %q", page.Main)
	}
	if page.MainTotalChars != 60 {
		t.Fatalf("main_total_chars = %d, want 60 runes", page.MainTotalChars)
	}

	rest := Window(read, ReadOptions{MaxChars: 50, Offset: page.NextOffset})
	if page.Main+rest.Main != read.Main {
		t.Fatal("rune-offset paging did not reassemble the original text")
	}
}

func TestWindowUnboundedReturnsWholeDocument(t *testing.T) {
	read := sampleRead()
	read.Main = strings.Repeat("x", DefaultReadMaxChars*2)

	got := Window(read, ReadOptions{MaxChars: UnboundedReadChars})
	if got.Main != read.Main {
		t.Fatalf("unbounded read returned %d chars, want %d", len(got.Main), len(read.Main))
	}
	if got.MainTruncated {
		t.Fatal("unbounded read reported truncation")
	}
}

func TestWindowOffsetPastEndReturnsEmptyNotPanic(t *testing.T) {
	got := Window(sampleRead(), ReadOptions{Offset: 5000})
	if got.Main != "" {
		t.Fatalf("prose = %q, want empty past the end", got.Main)
	}
	if got.MainTruncated {
		t.Fatal("past-the-end read reported truncation")
	}
}

func TestWindowIncludeSelectsSections(t *testing.T) {
	got := Window(sampleRead(), ReadOptions{Include: []string{"headings", "links"}})

	if got.Main != "" {
		t.Fatalf("prose returned despite not being included: %q", got.Main)
	}
	if len(got.Headings) != 2 || len(got.Links) != 2 {
		t.Fatalf("requested sections missing: %+v", got)
	}
	if len(got.Forms) != 0 || len(got.Tables) != 0 || got.Metadata.Lang != "" {
		t.Fatalf("unrequested sections returned: %+v", got)
	}
	// The count survives even when the prose itself is skipped, so an agent can
	// tell how much it chose not to fetch.
	if got.MainTotalChars != 100 {
		t.Fatalf("main_total_chars = %d, want 100", got.MainTotalChars)
	}
}

func TestWindowCapsListsAndFlagsTruncation(t *testing.T) {
	read := sampleRead()
	for i := 0; i < 400; i++ {
		read.Links = append(read.Links, Link{Text: "x", Href: "/x"})
	}

	got := Window(read, ReadOptions{MaxLinks: 5})
	if len(got.Links) != 5 {
		t.Fatalf("links = %d, want 5", len(got.Links))
	}
	if !got.LinksTruncated {
		t.Fatal("capped links not flagged as truncated")
	}

	uncapped := Window(read, ReadOptions{MaxLinks: UnboundedReadChars})
	if len(uncapped.Links) != len(read.Links) {
		t.Fatalf("uncapped links = %d, want %d", len(uncapped.Links), len(read.Links))
	}
}

func TestReadOptionsValidateRejectsUnknownSection(t *testing.T) {
	if err := (ReadOptions{Include: []string{"headings"}}).Validate(); err != nil {
		t.Fatalf("valid section rejected: %v", err)
	}
	err := (ReadOptions{Include: []string{"heading"}}).Validate()
	if err == nil {
		t.Fatal("typo'd section accepted; it would have returned a silently empty read")
	}
	if !strings.Contains(err.Error(), "heading") {
		t.Fatalf("error does not name the bad section: %v", err)
	}
}

func TestWindowDoesNotMutateInput(t *testing.T) {
	read := sampleRead()
	original := read.Main

	Window(read, ReadOptions{MaxChars: 5, Include: []string{"main"}})

	if read.Main != original {
		t.Fatal("Window mutated the caller's read")
	}
	if len(read.Headings) != 2 {
		t.Fatal("Window mutated the caller's headings")
	}
}

func TestNormalizeSectionsDeduplicates(t *testing.T) {
	got := NormalizeSections([]string{"Links", "links", " main ", ""})
	if len(got) != 2 || got[0] != "links" || got[1] != "main" {
		t.Fatalf("NormalizeSections = %v, want [links main]", got)
	}
}
