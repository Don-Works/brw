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

// getKinds maps each accepted question to whether it needs a target element.
var getKinds = map[string]bool{
	"url":      false,
	"title":    false,
	"text":     false, // whole-page text without a target
	"value":    true,
	"attr":     true,
	"count":    true,
	"box":      true,
	"styles":   true,
	"visible":  true,
	"hidden":   true,
	"enabled":  true,
	"disabled": true,
	"checked":  true,
}

// Validate reports a usable error for a malformed question, instead of letting
// the page script throw one an agent has to decode.
func (r GetRequest) Validate() error {
	what := strings.ToLower(strings.TrimSpace(r.What))
	needsTarget, known := getKinds[what]
	if !known {
		return fmt.Errorf("unknown get target %q: use one of %s", r.What, strings.Join(getKindNames(), ", "))
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

// Expression renders the in-page script for this question.
func (r GetRequest) Expression() string {
	return BuildGetExpression(strings.ToLower(strings.TrimSpace(r.What)), r.Target, r.Name)
}

func getKindNames() []string {
	names := make([]string, 0, len(getKinds))
	for name := range getKinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
