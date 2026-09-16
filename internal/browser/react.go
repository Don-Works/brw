package browser

import (
	"context"
	"fmt"
	"strings"
)

// ReactController is the optional capability for reading a page's React
// component tree. It reads the fiber tree React attaches to the DOM, which needs
// no page-side hook.
type ReactController interface {
	React(context.Context, ReactOptions) (ReactResult, error)
}

// ReactOptions selects a React introspection.
type ReactOptions struct {
	// Action is tree or inspect. renders and suspense are accepted and answered
	// with a note: they need the React DevTools hook, which brw does not inject.
	Action string `json:"action"`
	// Target names the element for action=inspect: a brw ref, or a CSS selector.
	Target string `json:"target,omitempty"`
	// Depth bounds how deep action=tree walks. Defaults to 30, capped at 100.
	Depth int `json:"depth,omitempty"`
	// Limit bounds how many nodes action=tree returns. Defaults to 300, capped at
	// 2000.
	Limit int    `json:"limit,omitempty"`
	TabID string `json:"tab_id,omitempty"`
}

// ReactNode is one component or host element in the tree.
type ReactNode struct {
	Depth int    `json:"depth"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Tag   string `json:"tag,omitempty"`
	Key   string `json:"key,omitempty"`
}

// ReactInspect is the component responsible for one element.
type ReactInspect struct {
	Component string            `json:"component"`
	Kind      string            `json:"kind,omitempty"`
	Key       string            `json:"key,omitempty"`
	Props     map[string]string `json:"props,omitempty"`
	HookKinds []string          `json:"hook_kinds,omitempty"`
	Ancestors []string          `json:"ancestors,omitempty"`
}

// ReactResult is the reply for a React introspection.
type ReactResult struct {
	OK      bool          `json:"ok"`
	Action  string        `json:"action"`
	TabID   string        `json:"tab_id,omitempty"`
	Present bool          `json:"present"`
	Nodes   []ReactNode   `json:"nodes,omitempty"`
	Count   int           `json:"count"`
	Inspect *ReactInspect `json:"inspect,omitempty"`
	Note    string        `json:"note,omitempty"`
}

// NormalizeReact validates a React request and resolves its defaults.
func NormalizeReact(opts ReactOptions) (ReactOptions, error) {
	out := opts
	out.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	if out.Action == "" {
		out.Action = "tree"
	}
	switch out.Action {
	case "tree", "inspect", "renders", "suspense":
	default:
		return out, fmt.Errorf("unknown react action %q: use tree, inspect, renders, or suspense", opts.Action)
	}
	if out.Action == "inspect" && strings.TrimSpace(opts.Target) == "" {
		return out, fmt.Errorf("react action=inspect needs target: a brw ref or a CSS selector")
	}
	if out.Depth == 0 {
		out.Depth = 30
	}
	if out.Depth < 1 || out.Depth > 100 {
		return out, fmt.Errorf("depth %d is outside the range 1-100", out.Depth)
	}
	if out.Limit == 0 {
		out.Limit = 300
	}
	if out.Limit < 1 || out.Limit > 2000 {
		return out, fmt.Errorf("limit %d is outside the range 1-2000", out.Limit)
	}
	return out, nil
}
