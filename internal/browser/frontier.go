package browser

import "github.com/Don-Works/brw/internal/snapshot"

func SelectFrontierElements(elements []snapshot.Element, focus string, limit int) []snapshot.Element {
	if limit <= 0 || len(elements) == 0 {
		return nil
	}
	out := make([]snapshot.Element, 0, limit)
	seen := map[string]bool{}
	add := func(el snapshot.Element) {
		if len(out) >= limit || el.Ref == "" || seen[el.Ref] {
			return
		}
		seen[el.Ref] = true
		out = append(out, el)
	}

	for _, el := range elements {
		if el.Ref == focus || hasSignal(el, "focused") || hasSignal(el, "focus-within") {
			add(el)
		}
	}
	for _, signal := range []string{
		"invalid",
		"expanded",
		"has-popup",
		"active-descendant",
		"active-descendant-owner",
		"live",
		"frontier-role",
	} {
		for _, el := range elements {
			if hasSignal(el, signal) {
				add(el)
			}
		}
	}
	if len(out) == 0 {
		for _, el := range elements {
			if el.Visible && el.InViewport {
				add(el)
			}
		}
	}
	return out
}

func hasSignal(el snapshot.Element, signal string) bool {
	for _, got := range el.Signals {
		if got == signal {
			return true
		}
	}
	return false
}
