package devtools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedTextCutsOnARuneBoundary(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "short text is unchanged", text: "contrast", limit: 10, want: "contrast"},
		{name: "exactly the limit is unchanged", text: "abcde", limit: 5, want: "abcde"},
		{name: "ascii cuts at the limit", text: "abcdefgh", limit: 5, want: "abcde"},

		{name: "a cut inside a two-byte rune drops the whole rune", text: "ééé", limit: 5, want: "éé"},

		{name: "a cut inside a four-byte rune drops the whole rune", text: "🔴🔴🔴", limit: 6, want: "🔴"},
		{name: "a zero limit yields nothing", text: "anything", limit: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundedText(tt.text, tt.limit)
			if got != tt.want {
				t.Fatalf("boundedText(%q, %d) = %q, want %q", tt.text, tt.limit, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("boundedText(%q, %d) = %q, which is not valid UTF-8", tt.text, tt.limit, got)
			}
		})
	}
}

func TestHighlightLabelSurvivesTruncation(t *testing.T) {

	label := "!" + strings.Repeat("é", MaxHighlightLabelBytes)
	opts, err := (HighlightOptions{Ref: "e1", Label: label}).Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(opts.Label) > MaxHighlightLabelBytes {
		t.Fatalf("label is %d bytes, want at most %d", len(opts.Label), MaxHighlightLabelBytes)
	}
	if !utf8.ValidString(opts.Label) {
		t.Fatalf("label = %q, which is not valid UTF-8", opts.Label)
	}
	if strings.ContainsRune(opts.Label, utf8.RuneError) {
		t.Fatalf("label = %q, want no replacement character from a mid-rune cut", opts.Label)
	}
}

func TestAuditSampleSurvivesTruncation(t *testing.T) {
	summary := "!" + strings.Repeat("é", auditSampleLimit)
	got := firstLine(summary)
	if len(got) > auditSampleLimit {
		t.Fatalf("sample is %d bytes, want at most %d", len(got), auditSampleLimit)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("sample = %q, want no replacement character from a mid-rune cut", got)
	}
}
