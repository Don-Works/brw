package browser

import (
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

// SelectOptionCandidate chooses the visible ARIA option that best matches value.
func SelectOptionCandidate(elements []snapshot.Element, value string) (snapshot.Element, bool) {
	want := normalizeOptionText(value)
	if want == "" {
		return snapshot.Element{}, false
	}
	bestScore := -1
	var best snapshot.Element
	for _, el := range elements {
		if el.Disabled {
			continue
		}
		if !el.Visible {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(el.Role))
		if role != "option" {
			continue
		}
		label := normalizeOptionText(optionText(el))
		if label == "" {
			continue
		}
		score := -1
		switch {
		case label == want:
			score = 100
		case strings.Contains(label, want):
			score = 70
		case strings.Contains(want, label):
			score = 45
		default:
			continue
		}
		if el.Visible {
			score += 30
		}
		if el.InViewport {
			score += 20
		}
		if el.Selected != nil && *el.Selected {
			score += 5
		}
		if score > bestScore {
			bestScore = score
			best = el
		}
	}
	return best, bestScore >= 0
}

func ElementMatchesOptionValue(el snapshot.Element, value string) bool {
	want := normalizeOptionText(value)
	if want == "" {
		return false
	}
	for _, candidate := range []string{el.Value, el.Name} {
		label := normalizeOptionText(candidate)
		if label == "" {
			continue
		}
		if label == want || strings.Contains(label, want) || strings.Contains(want, label) {
			return true
		}
	}
	return false
}

func optionText(el snapshot.Element) string {
	if strings.TrimSpace(el.Value) != "" {
		return el.Value
	}
	return el.Name
}

func normalizeOptionText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}
