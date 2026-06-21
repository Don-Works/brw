package snapshot

import (
	"fmt"
	"strings"
)

// compactValueLimit bounds per-element value/href text in the compact rendering
// so one verbose field cannot blow the token budget the format exists to save.
const compactValueLimit = 80

// RenderCompact renders a snapshot as one terse line per element instead of a
// JSON array — markedly fewer tokens for a small model while preserving the ref,
// role, name, and the state that drives decisions. Lines look like:
//
//	e17 button "Place order"
//	e3 textbox "Email" type=email =you@example.com required
//	e8 checkbox "Agree to terms" checked
//	e12 link "Docs" ->/docs
//	e30 visual:canvas "sales chart"
//
// A header carries the url/title; a footer carries counts, the snapshot version
// (for since-deltas), and any coverage / cross-origin hints from metadata.
func RenderCompact(snap PageSnapshot) string {
	var b strings.Builder
	if t := strings.TrimSpace(snap.Title); t != "" {
		fmt.Fprintf(&b, "# %s\n", t)
	}
	if snap.URL != "" {
		fmt.Fprintf(&b, "# %s\n", snap.URL)
	}
	if snap.Delta != nil {
		fmt.Fprintf(&b, "# delta: +%d ~%d -%d (added/changed/removed); lines below are the change set\n",
			len(snap.Delta.Added), len(snap.Delta.Changed), len(snap.Delta.Removed))
		if len(snap.Delta.Removed) > 0 {
			fmt.Fprintf(&b, "# removed: %s\n", strings.Join(snap.Delta.Removed, " "))
		}
	}
	for i := range snap.Elements {
		b.WriteString(compactElement(snap.Elements[i]))
		b.WriteByte('\n')
	}
	b.WriteString(compactFooter(snap.Metadata))
	return strings.TrimRight(b.String(), "\n")
}

func compactElement(el Element) string {
	var b strings.Builder
	b.WriteString(el.Ref)
	role := el.Role
	if isVisualSource(el.Source) && el.VisualType != "" {
		role = "visual:" + el.VisualType
	} else if role == "" {
		role = "element"
	}
	b.WriteByte(' ')
	b.WriteString(role)

	name := strings.TrimSpace(el.Name)
	if name == "" && el.VisualHint != "" {
		name = strings.TrimSpace(el.VisualHint)
	}
	if name != "" {
		fmt.Fprintf(&b, " %q", clip(name, compactValueLimit))
	}

	// type only when it adds information beyond the role (e.g. input type=email).
	if t := strings.TrimSpace(el.Type); t != "" && !strings.EqualFold(t, el.Role) {
		fmt.Fprintf(&b, " type=%s", t)
	}
	if v := strings.TrimSpace(el.Value); v != "" {
		if el.Sensitive {
			b.WriteString(" =***")
		} else {
			fmt.Fprintf(&b, " =%s", clip(v, compactValueLimit))
		}
	}
	if el.Href != "" {
		fmt.Fprintf(&b, " ->%s", clip(el.Href, compactValueLimit))
	}

	for _, flag := range compactFlags(el) {
		b.WriteByte(' ')
		b.WriteString(flag)
	}
	return b.String()
}

func compactFlags(el Element) []string {
	var flags []string
	if el.Disabled {
		flags = append(flags, "disabled")
	}
	if el.Required {
		flags = append(flags, "required")
	}
	if el.Checked != nil && *el.Checked {
		flags = append(flags, "checked")
	}
	if el.Selected != nil && *el.Selected {
		flags = append(flags, "selected")
	}
	if el.Expanded != nil {
		if *el.Expanded {
			flags = append(flags, "expanded")
		} else {
			flags = append(flags, "collapsed")
		}
	}
	if el.Valid != nil && !*el.Valid {
		flags = append(flags, "invalid")
	}
	if !el.Visible {
		flags = append(flags, "hidden")
	} else if !el.InViewport {
		flags = append(flags, "offscreen")
	}
	return flags
}

func compactFooter(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	var parts []string
	if v, ok := meta["element_count"]; ok {
		if tc, ok2 := meta["total_candidates"]; ok2 {
			parts = append(parts, fmt.Sprintf("%v/%v controls", v, tc))
		} else {
			parts = append(parts, fmt.Sprintf("%v controls", v))
		}
	}
	if v, ok := meta["version"]; ok {
		parts = append(parts, fmt.Sprintf("version %v", v))
	}
	if v, ok := meta["mode"].(string); ok && v != "" {
		parts = append(parts, v)
	}
	if v, ok := meta["truncated"].(bool); ok && v {
		parts = append(parts, "truncated — narrow with query/role or raise limit")
	}
	var b strings.Builder
	if len(parts) > 0 {
		fmt.Fprintf(&b, "# %s\n", strings.Join(parts, " · "))
	}
	if hint, ok := meta["coverage_hint"].(string); ok && hint != "" {
		fmt.Fprintf(&b, "# hint: %s\n", hint)
	}
	if note, ok := meta["cross_origin_note"].(string); ok && note != "" {
		fmt.Fprintf(&b, "# %s\n", note)
	}
	return b.String()
}

func isVisualSource(source []string) bool {
	for _, s := range source {
		if s == "visual" {
			return true
		}
	}
	return false
}

func clip(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " "))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
