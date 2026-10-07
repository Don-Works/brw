package main

import (
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

func (r *runner) resolveTabRef(name string) string {
	if id, ok := r.tabRefs[name]; ok {
		return id
	}
	return name
}

func (r *runner) runGroupTabsStep(st groupTabsStep) error {
	ids := make([]string, 0, len(st.Tabs))
	for _, name := range st.Tabs {
		ids = append(ids, r.resolveTabRef(name))
	}
	if len(ids) == 0 && r.tabID != "" {
		ids = append(ids, r.tabID)
	}
	if len(ids) == 0 {
		return fmt.Errorf("group_tabs needs tabs or a current tab")
	}

	body := map[string]any{"tab_ids": ids}
	if st.Title != "" {
		body["name"] = st.Title
	}
	if st.Color != "" {
		body["color"] = st.Color
	}
	if st.GroupID != "" {
		body["group_id"] = r.resolveTabRef(st.GroupID)
	}
	var result browser.ActionResult
	if err := r.client.postJSON("/api/browser/group_tabs", body, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("group_tabs reported not ok")
	}
	return nil
}

func (r *runner) runUngroupTabsStep(st ungroupTabsStep) error {
	ids := make([]string, 0, len(st.Tabs))
	for _, name := range st.Tabs {
		ids = append(ids, r.resolveTabRef(name))
	}
	if len(ids) == 0 && r.tabID != "" {
		ids = append(ids, r.tabID)
	}
	var result browser.ActionResult
	return r.client.postJSON("/api/browser/ungroup_tabs", map[string]any{"tab_ids": ids}, &result)
}

func (r *runner) runListTabGroupsStep(st listTabGroupsStep) error {
	var groups []struct {
		ID     string   `json:"id"`
		Title  string   `json:"title"`
		Color  string   `json:"color"`
		TabIDs []string `json:"tab_ids"`
	}
	if err := r.client.getJSON("/api/browser/tab_groups", &groups); err != nil {
		return err
	}
	if len(groups) < st.MinGroups {
		return fmt.Errorf("tab groups = %d, want at least %d", len(groups), st.MinGroups)
	}
	if st.WantTitle != "" {
		found := false
		for _, g := range groups {
			if g.Title != st.WantTitle {
				continue
			}
			found = true
			if st.WantColor != "" && g.Color != st.WantColor {
				return fmt.Errorf("group %q color = %q, want %q", g.Title, g.Color, st.WantColor)
			}
			if len(g.TabIDs) < st.MinMemberTabs {
				return fmt.Errorf("group %q has %d tabs, want at least %d", g.Title, len(g.TabIDs), st.MinMemberTabs)
			}
		}
		if !found {
			return fmt.Errorf("no tab group titled %q", st.WantTitle)
		}
	}
	if st.AbsentTitle != "" {
		for _, g := range groups {
			if g.Title == st.AbsentTitle {
				return fmt.Errorf("tab group %q should have been removed", st.AbsentTitle)
			}
		}
	}
	return nil
}

func (r *runner) runListTabsStep(st listTabsStep) error {
	tabs, err := r.client.listTabs()
	if err != nil {
		return err
	}
	if len(tabs) < st.MinTabs {
		return fmt.Errorf("tabs = %d, want at least %d", len(tabs), st.MinTabs)
	}
	if st.WantURL != "" {
		want := expandVars(st.WantURL)
		for _, t := range tabs {
			if strings.Contains(t.URL, want) {
				return nil
			}
		}
		return fmt.Errorf("no tab URL contains %q", want)
	}
	return nil
}

func (r *runner) runWindowBoundsStep(st windowBoundsStep) error {
	var bounds struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := r.client.getJSON(r.withTabQuery("/api/page/window_bounds"), &bounds); err != nil {
		return err
	}
	if bounds.Width < st.MinWidth {
		return fmt.Errorf("window width = %d, want at least %d", bounds.Width, st.MinWidth)
	}
	if bounds.Height < st.MinHeight {
		return fmt.Errorf("window height = %d, want at least %d", bounds.Height, st.MinHeight)
	}

	if st.WantWidth > 0 {
		if delta := bounds.Width - st.WantWidth; delta > 40 || delta < -40 {
			return fmt.Errorf("window width = %d, want ~%d", bounds.Width, st.WantWidth)
		}
	}
	return nil
}

func (r *runner) runMousePoint(path string, st mousePointStep) error {
	body := map[string]any{"x": st.X, "y": st.Y}
	if st.Button != "" {
		body["button"] = st.Button
	}
	r.addTabID(body)
	var result browser.ActionResult
	return r.client.postJSON(path, body, &result)
}

func (r *runner) runBatchStep(st batchStep) error {
	steps := make([]map[string]any, 0, len(st.Steps))
	for _, raw := range st.Steps {
		step := make(map[string]any, len(raw))
		for k, v := range raw {
			step[k] = v
		}
		if key, ok := step["ref"].(string); ok {
			if ref, found := r.refs[key]; found {
				step["ref"] = ref
			}
		}
		steps = append(steps, step)
	}
	body := map[string]any{"steps": steps}
	r.addTabID(body)
	var result browser.BatchResult
	err := r.client.postJSON("/api/page/batch", body, &result)
	if st.WantError {

		if err != nil || !result.OK || result.Error != "" {
			return nil
		}
		for _, s := range result.Steps {
			if !s.OK || s.Error != "" {
				return nil
			}
		}
		return fmt.Errorf("batch was expected to report an error and did not")
	}
	if err != nil {
		return err
	}
	for _, s := range result.Steps {
		if s.OK {
			continue
		}
		return fmt.Errorf("batch step %d (%s) failed: %s", s.Index, s.Action, s.Error)
	}
	ok := len(result.Steps)
	if ok < st.MinOK {
		return fmt.Errorf("batch ok steps = %d, want at least %d", ok, st.MinOK)
	}
	return nil
}

func (r *runner) runAssertStep(st assertStep) error {
	body := make(map[string]any, len(st.Assertion)+1)
	for key, value := range st.Assertion {
		body[key] = value
	}
	if key, ok := body["ref"].(string); ok {
		if ref, found := r.refs[key]; found {
			body["ref"] = ref
		}
	}
	if body["assertion"] == "url" {
		if expected, ok := body["expected"].(string); ok {
			body["expected"] = r.expandURL(expected)
		}
	}
	r.addTabID(body)
	var result browser.AssertResult
	err := r.client.postJSON("/api/page/assert", body, &result)
	if st.WantError {
		if err == nil {
			return fmt.Errorf("assertion %v was expected to fail and passed", body["assertion"])
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("assertion %q reported ok=false: expected %s, actual %s", result.Assertion, result.Expected, result.Actual)
	}
	return nil
}
