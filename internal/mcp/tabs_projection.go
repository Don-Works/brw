package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

type tabListRequest struct {
	Format string  `json:"format"`
	Query  *string `json:"query"`
	Limit  *int    `json:"limit"`
	Owned  *bool   `json:"owned"`
}

type tabListOptions struct {
	compact bool
	query   string
	limit   int
	owned   bool
}

func (r tabListRequest) options() (tabListOptions, error) {
	format := strings.ToLower(strings.TrimSpace(r.Format))
	if format != "" && format != "json" && format != "compact" {
		return tabListOptions{}, errors.New("format must be json or compact")
	}
	opts := tabListOptions{compact: format == "compact", limit: 40}
	if !opts.compact && (r.Query != nil || r.Limit != nil || r.Owned != nil) {
		return tabListOptions{}, errors.New("query, limit and owned require format compact")
	}
	if r.Query != nil {
		if utf8.RuneCountInString(*r.Query) > 256 {
			return tabListOptions{}, errors.New("query must not exceed 256 characters")
		}
		opts.query = strings.ToLower(strings.TrimSpace(*r.Query))
	}
	if r.Limit != nil {
		if *r.Limit < 1 || *r.Limit > 1000 {
			return tabListOptions{}, errors.New("limit must be between 1 and 1000")
		}
		opts.limit = *r.Limit
	}
	opts.owned = r.Owned != nil && *r.Owned
	return opts, nil
}

type compactTab struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Lease            string `json:"lease"`
	Active           bool   `json:"active,omitempty"`
	BrowserContextID string `json:"context_id,omitempty"`
	GroupDrift       bool   `json:"group_drift,omitempty"`
	ExpectedGroupID  string `json:"expected_group_id,omitempty"`
	Discarded        bool   `json:"discarded,omitempty"`
	Frozen           bool   `json:"frozen,omitempty"`
}

type compactTabList struct {
	Tabs             []compactTab `json:"tabs"`
	Total            int          `json:"total"`
	Matched          int          `json:"matched"`
	Returned         int          `json:"returned"`
	Truncated        bool         `json:"truncated"`
	OwnershipUnknown int          `json:"ownership_unknown"`
}

func tabLeaseStatus(tab browser.Tab) string {
	if tab.Lease != nil {
		switch tab.Lease.Status {
		case "mine", "leased", "available":
			return tab.Lease.Status
		}
	}
	return "unknown"
}

func projectTabList(tabs []browser.Tab, opts tabListOptions) compactTabList {
	result := compactTabList{Tabs: make([]compactTab, 0, min(opts.limit, len(tabs))), Total: len(tabs)}
	for _, tab := range tabs {
		lease := tabLeaseStatus(tab)
		if lease == "unknown" {
			result.OwnershipUnknown++
		}
		if opts.owned && lease != "mine" {
			continue
		}
		if opts.query != "" && !strings.Contains(strings.ToLower(tab.Title), opts.query) && !strings.Contains(strings.ToLower(tab.URL), opts.query) && !strings.Contains(strings.ToLower(tab.ID), opts.query) {
			continue
		}
		result.Matched++
		if len(result.Tabs) >= opts.limit {
			continue
		}
		projected := compactTab{ID: tab.ID, Title: tab.Title, URL: tab.URL, Lease: lease, Active: tab.Active, BrowserContextID: tab.BrowserContextID, Discarded: tab.Discarded, Frozen: tab.Frozen}
		if tab.Lease != nil {
			projected.GroupDrift = tab.Lease.GroupDrift
			projected.ExpectedGroupID = tab.Lease.ExpectedGroupID
		}
		result.Tabs = append(result.Tabs, projected)
	}
	result.Returned = len(result.Tabs)
	result.Truncated = result.Matched > result.Returned
	return result
}

func (s *Server) listTabs(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var req tabListRequest
	if err := unmarshalArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	opts, err := req.options()
	if err != nil {
		return nil, invalid(err)
	}
	observation := usagelog.ObservationFromContext(ctx)
	if observation != nil {
		observation.OutputFormat = "json"
		if opts.compact {
			observation.OutputFormat = "compact"
		}
	}
	tabs, err := s.manager.ListTabs(ctx)
	if owned, ok := s.pageWatch.(interface{ OwnsTab(string) bool }); ok {
		for i := range tabs {
			if owned.OwnsTab(tabs[i].ID) {
				tabs[i].Lease = &browser.TabLeaseInfo{Status: "leased"}
			}
		}
	}
	if !opts.compact || err != nil {
		return toolJSON(tabs, err)
	}
	projection := projectTabList(tabs, opts)
	if observation != nil {
		observation.ResultTruncated = usagelog.Flag(projection.Truncated)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{"content": []toolContent{{Type: "text", Text: string(data)}}}, nil
}
