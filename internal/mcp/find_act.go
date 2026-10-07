package mcp

import (
	"encoding/json"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

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
