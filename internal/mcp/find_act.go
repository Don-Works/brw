package mcp

import (
	"encoding/json"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// parseFindAct reads the locate-and-act half of a brw_find call: the action to
// run and the value to write. It reports whether an action was requested at all,
// so an ordinary read-only find is untouched.
//
// The search half is rebuilt from the already-decoded FindOptions rather than
// carried across, with one deliberate exception: limit is dropped. A caller who
// passes limit:1 would otherwise hand the exactly-one-match rule a set of one
// that the limit created, and the rule would confirm a uniqueness that is not
// there. browser.FindAct fixes its own limit for the same reason.
func parseFindAct(args json.RawMessage, opts snapshot.FindOptions) (browser.FindAct, bool, error) {
	var req struct {
		Action string `json:"action"`
		Value  string `json:"value"`
		Exact  bool   `json:"exact"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {
			return browser.FindAct{}, false, err
		}
	}
	if strings.TrimSpace(req.Action) == "" {
		return browser.FindAct{}, false, nil
	}
	findAct := browser.FindAct{
		Query:         opts.Query,
		Text:          opts.Text,
		Role:          opts.Role,
		Action:        strings.ToLower(strings.TrimSpace(req.Action)),
		Value:         req.Value,
		Exact:         req.Exact,
		ViewportOnly:  opts.ViewportOnly,
		IncludeHidden: opts.IncludeHidden,
		TextContent:   opts.TextContent,
	}
	if err := findAct.Validate(); err != nil {
		return browser.FindAct{}, true, err
	}
	return findAct, true, nil
}
