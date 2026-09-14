package snapshot

import (
	"slices"
	"testing"
)

// rankingPage deliberately puts the worst candidate first in document order, so
// a ranking that does nothing at all is distinguishable from one that works.
func rankingPage() []Element {
	return []Element{
		{Ref: "e4", Role: "textbox", Tag: "input", Name: "Invoice number", Disabled: true},
		{Ref: "e1", Role: "label", Tag: "label", Name: "Invoice number", Visible: true, InViewport: true},
		{Ref: "e2", Role: "textbox", Tag: "input", Name: "Invoice number", Visible: true, InViewport: true},
		{Ref: "e3", Role: "textbox", Tag: "input", Name: "Invoice number (draft)", Visible: true},
		{Ref: "e5", Role: "link", Tag: "a", Name: "Invoices", Href: "https://billing.example.test/invoices", Visible: true},
		{Ref: "e6", Role: "button", Tag: "button", Name: "Save", TestID: "save-invoice", Visible: true, InViewport: true},
	}
}

// A <label> carries the same accessible name as the field it labels and often
// precedes it, so document order offers the label first. Demoting it is the
// reason this is a ranking and not just a filter.
func TestRankTargetCandidatesDemotesALabelBelowTheFieldItNames(t *testing.T) {
	visible := true
	mixed := append([]Element{
		{Ref: "e7", Role: "textbox", Tag: "label", Name: "Invoice number", Visible: true, InViewport: true},
	}, rankingPage()...)
	order := refsOf(RankTargetCandidates(mixed, TargetCriteria{Role: "textbox", Name: "Invoice number", Visible: &visible}))
	if !slices.Equal(order, []string{"e2", "e7"}) {
		t.Fatalf("ranked %v; the label-tagged element leads in document order and must not lead the ranking", order)
	}
}

func TestRankTargetCandidatesPrefersTheStrongerMatchOverDocumentOrder(t *testing.T) {
	exact := refsOf(RankTargetCandidates(rankingPage(), TargetCriteria{Role: "textbox", Name: "Invoice number"}))
	if !slices.Equal(exact, []string{"e2", "e4"}) {
		t.Fatalf("exact-name candidates ranked %v; the visible enabled one must lead the disabled one that precedes it", exact)
	}
	substring := refsOf(RankTargetCandidates(rankingPage(), TargetCriteria{Role: "textbox", NameContains: "Invoice number"}))
	if !slices.Equal(substring, []string{"e2", "e3", "e4"}) {
		t.Fatalf("substring candidates ranked %v, want visible-and-enabled, visible, then neither", substring)
	}
}

func TestRankTargetCandidatesKeepsDocumentOrderForIndistinguishableElements(t *testing.T) {
	twins := []Element{
		{Ref: "a1", Role: "link", Name: "Open", Visible: true, InViewport: true},
		{Ref: "a2", Role: "link", Name: "Open", Visible: true, InViewport: true},
		{Ref: "a3", Role: "link", Name: "Open", Visible: true, InViewport: true},
	}
	first := refsOf(RankTargetCandidates(twins, TargetCriteria{Role: "link", Name: "Open"}))
	second := refsOf(RankTargetCandidates(twins, TargetCriteria{Role: "link", Name: "Open"}))
	if !slices.Equal(first, []string{"a1", "a2", "a3"}) || !slices.Equal(first, second) {
		t.Fatalf("ties ranked %v then %v; an unstable order makes a recorded ordinal meaningless", first, second)
	}
}

func TestTargetCriteriaMatches(t *testing.T) {
	yes, no := true, false
	page := rankingPage()
	tests := []struct {
		name     string
		criteria TargetCriteria
		want     []string
	}{
		{"role filters on its own", TargetCriteria{Role: "link"}, []string{"e5"}},
		{"exact name", TargetCriteria{Role: "textbox", Name: "Invoice number"}, []string{"e4", "e2"}},
		{"substring name", TargetCriteria{Role: "textbox", NameContains: "number (draft)"}, []string{"e3"}},
		{"test id", TargetCriteria{Role: "button", TestID: "save-invoice"}, []string{"e6"}},
		{"test id that no element carries", TargetCriteria{Role: "button", TestID: "delete-invoice"}, nil},
		{"href fragment", TargetCriteria{Role: "link", HrefContains: "/invoices"}, []string{"e5"}},
		{"href fragment that is not present", TargetCriteria{Role: "link", HrefContains: "/statements"}, nil},
		{"visible only", TargetCriteria{Role: "textbox", Name: "Invoice number", Visible: &yes}, []string{"e2"}},
		{"hidden only", TargetCriteria{Role: "textbox", Name: "Invoice number", Visible: &no}, []string{"e4"}},
		{"role must match too", TargetCriteria{Role: "button", Name: "Invoice number"}, nil},
		{"every populated criterion must hold", TargetCriteria{Role: "button", TestID: "save-invoice", Name: "Delete"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			for _, element := range page {
				if test.criteria.Matches(element) {
					got = append(got, element.Ref)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("matched %v, want %v", got, test.want)
			}
		})
	}
}

func refsOf(elements []Element) []string {
	refs := make([]string, 0, len(elements))
	for _, element := range elements {
		refs = append(refs, element.Ref)
	}
	return refs
}
