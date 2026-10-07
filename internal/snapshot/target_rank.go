package snapshot

import (
	"sort"
	"strings"
)

// TargetCriteria is one semantic element identity: a role plus at least one stable name or attribute.
type TargetCriteria struct {
	Role         string
	Name         string
	NameContains string
	TestID       string
	HrefContains string
	// Visible, when set, requires the element's visibility to equal it.
	Visible *bool
}

// Matches reports whether element satisfies every populated criterion.
func (c TargetCriteria) Matches(element Element) bool {
	switch {
	case element.Role != c.Role:
		return false
	case c.Name != "" && element.Name != c.Name:
		return false
	case c.NameContains != "" && !strings.Contains(element.Name, c.NameContains):
		return false
	case c.TestID != "" && element.TestID != c.TestID:
		return false
	case c.HrefContains != "" && !strings.Contains(element.Href, c.HrefContains):
		return false
	case c.Visible != nil && element.Visible != *c.Visible:
		return false
	}
	return true
}

// RankTargetCandidates returns every element the criteria match, best candidate first, without mutating elements.
func RankTargetCandidates(elements []Element, c TargetCriteria) []Element {
	type ranked struct {
		element Element
		score   int
	}
	candidates := make([]ranked, 0, len(elements))
	for index := range elements {
		if !c.Matches(elements[index]) {
			continue
		}
		candidates = append(candidates, ranked{
			element: elements[index],
			score:   targetCandidateScore(elements[index]),
		})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].score > candidates[right].score
	})
	out := make([]Element, len(candidates))
	for index := range candidates {
		out[index] = candidates[index].element
	}
	return out
}

func targetCandidateScore(element Element) int {
	score := 0
	if element.Role == "label" || element.Tag == "label" {
		score -= 60
	}
	if element.Visible {
		score += 20
	}
	if element.InViewport {
		score += 15
	}
	if !element.Disabled {
		score += 10
	}
	return score
}
