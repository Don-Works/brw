package snapshot

import (
	"sort"
	"strings"
)

// TargetCriteria is one semantic element identity: a role plus at least one
// stable name or attribute. It lives beside Element rather than in the recipe
// package because two callers need exactly the same answer to "which elements
// does this identity name" — the runtime resolver, which asks a live page, and
// the trace compiler, which asks a recorded observation. Two copies of the
// predicate would let a target compile as unambiguous and then resolve to two
// elements at run time.
type TargetCriteria struct {
	Role         string
	Name         string
	NameContains string
	TestID       string
	HrefContains string
	// Visible, when set, requires the element's visibility to equal it. Nil
	// means visibility is not part of the identity.
	Visible *bool
}

// Matches reports whether element satisfies every populated criterion. Role is
// always required: a name on its own matches a label as readily as the field it
// labels.
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

// RankTargetCandidates returns every element the criteria match, best candidate
// first, without mutating elements.
//
// The ordering carries the same tie-breaks the in-page find ranking applies
// once an exact role filter has already been imposed: a <label> is demoted
// because it is the classic wrong answer for a field identity, an exact
// accessible name outranks a substring hit, and a visible, in-viewport,
// enabled element outranks one that is none of those. Ties keep document
// order, so ranking the same observation twice gives the same list.
//
// Ranking does not decide anything on its own. A caller that needs one element
// must still check that exactly one candidate came back; this only says which
// candidate a page would have offered first, which is what makes a recorded
// action's ordinal meaningful.
func RankTargetCandidates(elements []Element, c TargetCriteria) []Element {
	type ranked struct {
		element  Element
		score    int
		position int
	}
	candidates := make([]ranked, 0, len(elements))
	for index := range elements {
		if !c.Matches(elements[index]) {
			continue
		}
		candidates = append(candidates, ranked{
			element:  elements[index],
			score:    targetCandidateScore(elements[index], c),
			position: index,
		})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].score != candidates[right].score {
			return candidates[left].score > candidates[right].score
		}
		return candidates[left].position < candidates[right].position
	})
	out := make([]Element, len(candidates))
	for index := range candidates {
		out[index] = candidates[index].element
	}
	return out
}

func targetCandidateScore(element Element, c TargetCriteria) int {
	score := 0
	if c.Name != "" && element.Name == c.Name {
		score += 100
	}
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
