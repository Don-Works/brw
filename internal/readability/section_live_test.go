package readability

import (
	"strings"
	"testing"
)

// The offsets are computed in-page by searching the extracted prose for each
// heading's text. That only works while both strings come from the same
// whitespace-collapsing helper, so it has to be proven against a real render
// rather than a hand-built fixture.
func TestLiveHeadingOffsetsAddressRealProse(t *testing.T) {
	html := `<!DOCTYPE html><html><body><main>
<h1>Getting  started</h1>
<p>First paragraph of the intro.</p>
<h2>Install</h2>
<p>Run the installer and wait.</p>
<h3>Linux</h3>
<p>Use the package manager.</p>
<h2>Usage</h2>
<p>Call the binary with a flag.</p>
</main></body></html>`

	ctx, cancel := readTestContext(t)
	defer cancel()
	read := navigateRead(t, ctx, html)

	if len(read.Headings) != 4 {
		t.Fatalf("headings = %+v, want 4", read.Headings)
	}

	runes := []rune(read.Main)
	for _, heading := range read.Headings {
		if heading.Offset == nil {
			t.Errorf("heading %q carries no offset at all; section addressing would be unavailable", heading.Text)
			continue
		}
		offset := *heading.Offset
		if offset < 0 {
			t.Errorf("heading %q got no offset into the extracted prose", heading.Text)
			continue
		}
		if offset > len(runes) {
			t.Errorf("heading %q offset %d is past the end of the prose (%d)", heading.Text, offset, len(runes))
			continue
		}
		// The offset must land exactly on the heading's own text, or every
		// section slice is off by however far it drifted.
		at := string(runes[offset:min(offset+len([]rune(heading.Text)), len(runes))])
		if at != heading.Text {
			t.Errorf("offset %d for heading %q lands on %q", offset, heading.Text, at)
		}
	}

	// Whitespace in the source heading ("Getting  started") is collapsed by the
	// same helper on both sides, so the match still lands.
	if read.Headings[0].Text != "Getting started" {
		t.Fatalf("heading text = %q, want whitespace collapsed", read.Headings[0].Text)
	}

	span, ok := FindSectionSpan(read.Headings, len(runes), "Install")
	if !ok {
		t.Fatal("Install section not addressable on a real render")
	}
	got := string(runes[span.Start:span.End])
	if !strings.Contains(got, "Run the installer") {
		t.Errorf("Install section missing its own prose: %q", got)
	}
	if !strings.Contains(got, "package manager") {
		t.Errorf("Install section dropped its h3 subsection: %q", got)
	}
	if strings.Contains(got, "Call the binary") {
		t.Errorf("Install section ran into Usage: %q", got)
	}
}
