package snapshot

import (
	"fmt"
	"sort"
	"strings"
)

// GetRequest is one typed question about the page or one element, as the MCP
// tool and the HTTP route both receive it. Validating here rather than at each
// surface is what keeps the two from drifting into different accepted vocabularies.
type GetRequest struct {
	What   string `json:"what"`
	Target string `json:"target,omitempty"`
	Name   string `json:"name,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// Validate reports a usable error for a malformed question, instead of letting
// the page script throw one an agent has to decode.
func (r GetRequest) Validate() error {
	what := strings.ToLower(strings.TrimSpace(r.What))
	needsTarget, known := getKinds[what]
	if !known {
		return fmt.Errorf("unknown get target %q: use one of %s", r.What, strings.Join(GetKindNames(), ", "))
	}
	if needsTarget && strings.TrimSpace(r.Target) == "" {
		if what == "count" {
			return fmt.Errorf("get what=count requires target: the CSS selector to count")
		}
		return fmt.Errorf("get what=%s requires target: a brw ref or CSS selector", what)
	}
	if what == "attr" && strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("get what=attr requires name: the attribute to read")
	}
	return nil
}

// TraceLabel renders this question the way an operator reading brw_trace needs
// to see it — what was asked of which target — rather than the ~10 KB walker
// expression that answers it.
func (r GetRequest) TraceLabel() string {
	label := strings.ToLower(strings.TrimSpace(r.What))
	if target := strings.TrimSpace(r.Target); target != "" {
		label += " " + target
	}
	if name := strings.TrimSpace(r.Name); name != "" {
		label += " " + name
	}
	return label
}

// Expression renders the in-page script for this question.
func (r GetRequest) Expression() string {
	return BuildGetExpression(strings.ToLower(strings.TrimSpace(r.What)), r.Target, r.Name)
}

// GetKindNames lists the accepted questions in sorted order. The MCP tool's
// enum is built from it rather than from a hand-kept literal, so a kind the
// schema advertises is always one Validate accepts.
func GetKindNames() []string {
	names := make([]string, 0, len(getKinds))
	for name := range getKinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
