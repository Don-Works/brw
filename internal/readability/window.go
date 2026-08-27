package readability

import (
	"fmt"
	"sort"
	"strings"
)

// Defaults for a bounded page read. A read used to be unbounded up to the
// in-page 100k-character clip, which lands ~25k tokens of prose in an agent's
// context from a single call. Bounding by default with explicit paging metadata
// keeps the common case cheap while leaving the whole document reachable.
const (
	DefaultReadMaxChars    = 20000
	DefaultReadMaxLinks    = 300
	DefaultReadMaxHeadings = 100
)

// UnboundedReadChars is the sentinel for "return the whole document", for
// callers that genuinely want every character in one response.
const UnboundedReadChars = -1

// ReadSections are the selectable parts of a page read. An agent that only
// wants navigation can ask for headings+links and skip the prose entirely.
var ReadSections = []string{"main", "headings", "links", "forms", "tables", "metadata"}

// ReadOptions bounds what Window keeps from a full page read.
type ReadOptions struct {
	// MaxChars caps the returned prose. Zero selects DefaultReadMaxChars;
	// UnboundedReadChars returns everything.
	MaxChars int `json:"max_chars,omitempty"`
	// Offset is the rune offset into the prose, for paging with NextOffset.
	Offset int `json:"offset,omitempty"`
	// Include selects sections by name. Empty means every section.
	Include []string `json:"include,omitempty"`
	// Section names a heading; the prose returned is that heading's span, ending
	// at the next heading of the same or higher level. Applied before MaxChars
	// and Offset, which then page within the section.
	Section string `json:"section,omitempty"`
	// MaxLinks and MaxHeadings cap their lists. Zero selects the defaults.
	MaxLinks    int `json:"max_links,omitempty"`
	MaxHeadings int `json:"max_headings,omitempty"`
}

// Validate reports unknown section names rather than silently dropping them, so
// a typo surfaces as an error instead of a quietly empty read.
func (o ReadOptions) Validate() error {
	for _, name := range o.Include {
		if !validSection(name) {
			return fmt.Errorf("unknown include section %q (valid: %s)", name, strings.Join(ReadSections, ", "))
		}
	}
	return nil
}

func validSection(name string) bool {
	for _, known := range ReadSections {
		if strings.EqualFold(strings.TrimSpace(name), known) {
			return true
		}
	}
	return false
}

func (o ReadOptions) wants(section string) bool {
	if len(o.Include) == 0 {
		return true
	}
	for _, name := range o.Include {
		if strings.EqualFold(strings.TrimSpace(name), section) {
			return true
		}
	}
	return false
}

// SectionSpan is the character range one heading owns within a document's prose.
type SectionSpan struct {
	Heading string
	Level   int
	Start   int
	End     int
}

// addressableOffset returns a heading's position within the prose, and whether
// it can be addressed at all. A nil offset means the read came from a source
// that does not compute them; a negative one means the heading sits outside the
// extracted prose. Neither can be sliced from.
func addressableOffset(heading Heading) (int, bool) {
	if heading.Offset == nil || *heading.Offset < 0 {
		return 0, false
	}
	return *heading.Offset, true
}

// SectionsAddressable reports whether a read carries the heading offsets that
// section selection needs. False means the caller should say so rather than
// return a span it cannot compute.
func SectionsAddressable(headings []Heading) bool {
	for _, heading := range headings {
		if _, ok := addressableOffset(heading); ok {
			return true
		}
	}
	return false
}

// FindSectionSpan locates the span a heading owns: from the heading itself to
// the next heading of the same or higher level, or the end of the prose. A
// heading with no addressable offset is skipped. Matching is case-insensitive
// on the trimmed heading text, and prefers an exact match over a substring one
// so "Install" does not silently select "Installation" when both exist.
func FindSectionSpan(headings []Heading, totalRunes int, name string) (SectionSpan, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" || totalRunes == 0 {
		return SectionSpan{}, false
	}

	best := -1
	bestStart := 0
	for i, heading := range headings {
		offset, ok := addressableOffset(heading)
		if !ok {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(heading.Text))
		if text == want {
			best, bestStart = i, offset
			break
		}
		if best == -1 && strings.Contains(text, want) {
			best, bestStart = i, offset
		}
	}
	if best == -1 {
		return SectionSpan{}, false
	}

	// The section ends at the NEAREST following sibling-or-shallower heading,
	// chosen by offset rather than by position in the slice. A headings payload
	// that arrives out of document order would otherwise produce a span that
	// overruns its neighbour.
	end := totalRunes
	for _, later := range headings {
		offset, ok := addressableOffset(later)
		if !ok || offset <= bestStart || offset >= end {
			continue
		}
		// A deeper heading is part of this section; only a sibling or an
		// ancestor ends it.
		if later.Level <= headings[best].Level {
			end = offset
		}
	}
	if end > totalRunes {
		end = totalRunes
	}
	if bestStart > end {
		return SectionSpan{}, false
	}
	return SectionSpan{Heading: headings[best].Text, Level: headings[best].Level, Start: bestStart, End: end}, true
}

// SectionNames lists the addressable headings, for an error that tells a caller
// what it could have asked for instead.
func SectionNames(headings []Heading) []string {
	out := make([]string, 0, len(headings))
	for _, heading := range headings {
		if _, ok := addressableOffset(heading); ok && strings.TrimSpace(heading.Text) != "" {
			out = append(out, heading.Text)
		}
	}
	return out
}

// Window returns a bounded copy of read. It never mutates the input.
func Window(read PageRead, opts ReadOptions) PageRead {
	out := read

	if opts.wants("main") {
		prose := read.Main
		if opts.Section != "" {
			// A section that cannot be found is reported by the caller as an
			// argument error; Window falls back to the whole document rather
			// than inventing an empty one.
			if span, ok := FindSectionSpan(read.Headings, len([]rune(read.Main)), opts.Section); ok {
				runes := []rune(read.Main)
				prose = string(runes[span.Start:span.End])
				out.Section = span.Heading
				out.SectionLevel = span.Level
			}
		}
		out.Main, out.MainTotalChars, out.MainTruncated, out.NextOffset = windowText(prose, opts)
	} else {
		out.Main = ""
		out.MainTotalChars = len([]rune(read.Main))
	}

	if !opts.wants("headings") {
		out.Headings = nil
	} else {
		out.Headings, out.HeadingsTruncated = capHeadings(read.Headings, limitOr(opts.MaxHeadings, DefaultReadMaxHeadings))
	}
	if !opts.wants("links") {
		out.Links = nil
	} else {
		out.Links, out.LinksTruncated = capLinks(read.Links, limitOr(opts.MaxLinks, DefaultReadMaxLinks))
	}
	if !opts.wants("forms") {
		out.Forms = nil
	}
	if !opts.wants("tables") {
		out.Tables = nil
	}
	if !opts.wants("metadata") {
		out.Metadata = Metadata{}
	}
	return out
}

// windowText slices prose on rune boundaries so a multi-byte character is never
// split across the cut, and reports what was left behind.
func windowText(text string, opts ReadOptions) (windowed string, total int, truncated bool, nextOffset int) {
	runes := []rune(text)
	total = len(runes)

	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return "", total, false, 0
	}

	limit := opts.MaxChars
	switch {
	case limit == UnboundedReadChars:
		return string(runes[offset:]), total, false, 0
	case limit <= 0:
		limit = DefaultReadMaxChars
	}

	// Compare against the remaining length rather than computing offset+limit
	// first: a caller-supplied max_chars near MaxInt overflows that addition to a
	// negative number, and the slice that follows panics the daemon. offset is
	// already known to be < total here, so total-offset cannot underflow.
	if limit >= total-offset {
		return string(runes[offset:]), total, false, 0
	}
	end := offset + limit
	return string(runes[offset:end]), total, true, end
}

func limitOr(value, fallback int) int {
	if value == UnboundedReadChars {
		return UnboundedReadChars
	}
	if value <= 0 {
		return fallback
	}
	return value
}

func capHeadings(items []Heading, limit int) ([]Heading, bool) {
	if limit == UnboundedReadChars || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

func capLinks(items []Link, limit int) ([]Link, bool) {
	if limit == UnboundedReadChars || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

// NormalizeSections lowercases and de-duplicates section names, preserving a
// stable order so identical requests produce identical responses.
func NormalizeSections(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		clean := strings.ToLower(strings.TrimSpace(name))
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	sort.Strings(out)
	return out
}
