package readability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Defaults for a bounded page read.
const (
	DefaultReadMaxChars    = 20000
	DefaultReadMaxLinks    = 300
	DefaultReadMaxHeadings = 100
)

// UnboundedReadChars is the sentinel for "return the whole document", for callers that genuinely want every character in one response.
const UnboundedReadChars = -1

// ReadSections are the selectable parts of a page read.
var ReadSections = []string{"main", "headings", "links", "forms", "tables", "metadata"}

// ReadOptions bounds what Window keeps from a full page read.
type ReadOptions struct {
	SettleMS *int `json:"settle_ms,omitempty"`
	// MaxChars caps the returned prose.
	MaxChars int `json:"max_chars,omitempty"`
	// Offset is the rune offset into the prose, for paging with NextOffset.
	Offset int `json:"offset,omitempty"`
	// Include selects sections by name.
	Include SectionList `json:"include,omitempty"`
	// Section names a heading; the prose returned is that heading's span, ending at the next heading of the same or higher level.
	Section string `json:"section,omitempty"`
	// MaxLinks and MaxHeadings cap their lists.
	MaxLinks    int `json:"max_links,omitempty"`
	MaxHeadings int `json:"max_headings,omitempty"`
}

// SectionList is the include parameter's wire form.
type SectionList []string

func (l *SectionList) UnmarshalJSON(data []byte) error {
	var names []string
	if err := json.Unmarshal(data, &names); err == nil {
		*l = names
		return nil
	}
	var joined string
	if err := json.Unmarshal(data, &joined); err != nil {
		return fmt.Errorf("include must be an array of section names or a comma-separated string")
	}
	*l = strings.Split(joined, ",")
	return nil
}

// Validate reports unknown section names rather than silently dropping them, so a typo surfaces as an error instead of a quietly empty read.
func (o ReadOptions) Validate() error {
	if o.SettleMS != nil && (*o.SettleMS < 0 || *o.SettleMS > 5000) {
		return fmt.Errorf("settle_ms must be an integer from 0 to 5000")
	}
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

func addressableOffset(heading Heading) (int, bool) {
	if heading.Offset == nil || *heading.Offset < 0 {
		return 0, false
	}
	return *heading.Offset, true
}

// SectionsAddressable reports whether a read carries the heading offsets that section selection needs.
func SectionsAddressable(headings []Heading) bool {
	for _, heading := range headings {
		if _, ok := addressableOffset(heading); ok {
			return true
		}
	}
	return false
}

// FindSectionSpan locates the span a heading owns: from the heading itself to the next heading of the same or higher level, or the end of the prose.
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

	end := totalRunes
	for _, later := range headings {
		offset, ok := addressableOffset(later)
		if !ok || offset <= bestStart || offset >= end {
			continue
		}

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

// SectionNames lists the addressable headings, for an error that tells a caller what it could have asked for instead.
func SectionNames(headings []Heading) []string {
	out := make([]string, 0, len(headings))
	for _, heading := range headings {
		if _, ok := addressableOffset(heading); ok && strings.TrimSpace(heading.Text) != "" {
			out = append(out, heading.Text)
		}
	}
	return out
}

// Window returns a bounded copy of read.
func Window(read PageRead, opts ReadOptions) PageRead {
	out := read

	if opts.wants("main") {
		prose := read.Main
		if opts.Section != "" {

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
		out.Headings, out.HeadingsTruncated = capItems(read.Headings, limitOr(opts.MaxHeadings, DefaultReadMaxHeadings))
	}
	if !opts.wants("links") {
		out.Links = nil
	} else {
		out.Links, out.LinksTruncated = capItems(read.Links, limitOr(opts.MaxLinks, DefaultReadMaxLinks))
	}
	if !opts.wants("forms") {
		out.Forms = nil
	}
	if !opts.wants("tables") {
		out.Tables = nil
		out.TablesComplete = false
		out.TablesTruncated = false
	}
	if !opts.wants("main") || !opts.wants("headings") || out.MainTruncated || out.HeadingsTruncated || out.Section != "" {
		out.SectionsAnchored = false
	}
	if !opts.wants("metadata") {
		out.Metadata = Metadata{}
	}
	return out
}

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

func capItems[T any](items []T, limit int) ([]T, bool) {
	if limit == UnboundedReadChars || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

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
