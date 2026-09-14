package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/snapshot"
)

// errBaselinesDisabled is the honest answer from a daemon with no baseline
// root. Silently keeping baselines in a temporary directory would make a gate
// that passes on every fresh process, which is worse than not having one.
var errBaselinesDisabled = errors.New("regression baselines are not enabled on this daemon; start brwd with --baseline-root pointing at a directory outside any repository")

// SetBaselineStore installs the regression-baseline store.
func (s *Server) SetBaselineStore(store *baseline.Store) { s.baselines = store }

// baselineEnvironmentExpression reports the capture conditions that belong in a
// baseline key, from inside the page, so every transport can answer it. The
// operating system is not in here: it is the browser HOST's, which the daemon
// knows and the page does not (a page's platform string is spoofable and, under
// the extension bridge, says nothing about where the daemon runs).
const baselineEnvironmentExpression = `(function(){
  var ua = navigator.userAgent || '';
  var build = (ua.match(/(?:Chrome|Chromium|Edg|Firefox|Version)\/[0-9][0-9.]*/) || [''])[0];
  return {
    browser_build: build || ua.slice(0, 120),
    viewport_width: Math.round(window.innerWidth || 0),
    viewport_height: Math.round(window.innerHeight || 0),
    device_pixel_ratio: window.devicePixelRatio || 1,
    locale: navigator.language || ''
  };
})()`

type baselineRequest struct {
	Action           string                  `json:"action"`
	RecipeDigest     string                  `json:"recipe_digest"`
	StepIndex        int                     `json:"step_index"`
	PixelTolerance   float64                 `json:"pixel_tolerance,omitempty"`
	ChannelTolerance int                     `json:"channel_tolerance,omitempty"`
	IgnoreRegions    []baseline.IgnoreRegion `json:"ignore_regions,omitempty"`
	TabID            string                  `json:"tab_id,omitempty"`
}

// baselineListResult answers "what do I already have for this step, and under
// what conditions was it taken".
type baselineListResult struct {
	Action       string                 `json:"action"`
	RecipeDigest string                 `json:"recipe_digest"`
	StepIndex    int                    `json:"step_index"`
	Environments []baseline.Environment `json:"environments"`
	Count        int                    `json:"count"`
	Note         string                 `json:"note,omitempty"`
}

func (s *Server) callBaseline(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	if s.baselines == nil {
		return toolError(errBaselinesDisabled), nil
	}
	var req baselineRequest
	if err := unmarshalStrictArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if req.ChannelTolerance < 0 || req.ChannelTolerance > 255 {
		return toolError(errors.New("channel_tolerance must be between 0 and 255")), nil
	}
	if req.PixelTolerance < 0 || req.PixelTolerance > 1 {
		return toolError(errors.New("pixel_tolerance is a fraction of compared pixels, between 0 and 1")), nil
	}

	switch action {
	case "list":
		environments, err := s.baselines.EnvironmentsFor(req.RecipeDigest, req.StepIndex)
		if err != nil {
			return toolError(err), nil
		}
		if environments == nil {
			environments = []baseline.Environment{}
		}
		result := baselineListResult{
			Action: action, RecipeDigest: req.RecipeDigest, StepIndex: req.StepIndex,
			Environments: environments, Count: len(environments),
		}
		if len(environments) == 0 {
			result.Note = "no baseline is stored for this recipe step in any environment"
		}
		return toolJSON(result, nil)

	case "check", "update":
		environment, err := s.baselineEnvironment(ctx)
		if err != nil {
			return toolError(err), nil
		}
		key := baseline.Key{RecipeDigest: req.RecipeDigest, StepIndex: req.StepIndex, Environment: environment}
		if err := key.Validate(); err != nil {
			return toolError(err), nil
		}
		shot, err := s.manager.Screenshot(ctx)
		if err != nil {
			return toolError(err), nil
		}
		pixels := shot.Data
		if len(pixels) == 0 && shot.Base64 != "" {
			pixels, err = base64.StdEncoding.DecodeString(shot.Base64)
			if err != nil {
				return toolError(fmt.Errorf("decode screenshot: %w", err)), nil
			}
		}
		tree, err := s.ariaTree(ctx)
		if err != nil {
			return toolError(err), nil
		}
		result, err := baseline.Check(s.baselines, baseline.CheckOptions{
			Key:              key,
			Screenshot:       pixels,
			Tree:             tree,
			IgnoreRegions:    req.IgnoreRegions,
			PixelTolerance:   req.PixelTolerance,
			ChannelTolerance: uint8(req.ChannelTolerance),
			// This is the only place update is ever true, and it is set from the
			// action the caller named — never inferred from a passing run.
			Update: action == "update",
		})
		if err != nil {
			return toolError(err), nil
		}
		return toolJSON(result, nil)

	case "delete":
		environment, err := s.baselineEnvironment(ctx)
		if err != nil {
			return toolError(err), nil
		}
		key := baseline.Key{RecipeDigest: req.RecipeDigest, StepIndex: req.StepIndex, Environment: environment}
		if err := s.baselines.Delete(key); err != nil {
			return toolError(err), nil
		}
		return toolJSON(map[string]any{"action": action, "baseline_id": key.ID(), "note": "baseline deleted"}, nil)

	case "":
		return toolError(errors.New(`action is required: "check", "update", "list", or "delete"`)), nil
	}
	return toolError(fmt.Errorf("unknown action %q: want check, update, list, or delete", req.Action)), nil
}

func (s *Server) baselineEnvironment(ctx context.Context) (baseline.Environment, error) {
	raw, err := s.manager.Evaluate(ctx, baselineEnvironmentExpression)
	if err != nil {
		return baseline.Environment{}, fmt.Errorf("read the environment fingerprint from the page: %w", err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return baseline.Environment{}, err
	}
	var environment baseline.Environment
	if err := json.Unmarshal(encoded, &environment); err != nil {
		return baseline.Environment{}, fmt.Errorf("decode the environment fingerprint: %w", err)
	}
	environment.OS = runtime.GOOS
	return environment.Normalize(), nil
}

func (s *Server) ariaTree(ctx context.Context) (snapshot.AriaTree, error) {
	raw, err := s.manager.Evaluate(ctx, snapshot.AriaTreeExpression)
	if err != nil {
		return snapshot.AriaTree{}, fmt.Errorf("read the page's ARIA structure: %w", err)
	}
	return snapshot.ParseAriaTree(raw)
}

// baselineTool is the catalogue entry. Every guarantee in it is enforced in
// callBaseline or internal/baseline: the environment is part of the key, an
// update only happens on the update action, and nothing is written by a check.
func baselineTool() map[string]any {
	return tool("brw_baseline", "Gate a page against a stored regression baseline instead of eyeballing a screenshot. Actions: check (compare the current page against the baseline for this recipe step and environment — WRITES NOTHING, ever, including when it passes), update (replace that baseline with what the page looks like now; this is the only way a baseline changes), list (the environments a baseline already exists under for this step), delete (drop one baseline). A baseline is keyed by recipe_digest + step_index + an environment fingerprint brw measures itself: browser build, viewport, device pixel ratio, locale and the browser host's OS. If a baseline exists for the step but under different conditions, check reports status \"environment_mismatch\" and names the fields that moved (\"device_pixel_ratio 1 -> 2\") instead of a screen full of false pixel differences. Two comparisons run. The visual one counts pixels that moved, with pixel_tolerance as the fraction of compared pixels allowed to differ, channel_tolerance as the per-channel slack that absorbs anti-aliasing, and ignore_regions as NAMED rectangles in CSS pixels — put the clock, the avatar and the ad slot in there or every run fails. The structural one diffs the page's ARIA structure, role and accessible name, which catches a button losing its label — invisible to a pixel diff. It carries no geometry, so a change that only repaints (a font bump, a colour) moves the visual half and leaves the structural half alone. status is \"match\", \"diff\", \"environment_mismatch\", \"missing\" (nothing stored yet; run update once to record it), \"recorded\" or \"updated\"; failed is the single boolean to branch on. Baselines are stored by the daemon that runs the check, under a root the operator configures, and that daemon refuses a root inside a Git working tree — so a baseline of a signed-in page cannot end up committed.", object(map[string]any{
		"action":        stringEnumSchema("check (compare, never write), update (accept the current page as the new baseline), list (environments already recorded for this step), delete (remove this environment's baseline).", "check", "update", "list", "delete"),
		"recipe_digest": stringSchema("The 64-character hex content digest of the pinned recipe version, as returned by brw_recipe_search. Editing a recipe changes its digest, which orphans its baselines rather than silently comparing new behaviour against the old."),
		"step_index":    integerSchema("Zero-based index of the step in that recipe this baseline belongs to."),
		"pixel_tolerance": map[string]any{
			"type":        "number",
			"description": "Fraction of compared pixels allowed to differ before the visual check fails, 0 to 1. Defaults to 0.",
		},
		"channel_tolerance": integerSchema("Per-channel 8-bit difference treated as identical, 0 to 255. A small value (2-4) absorbs anti-aliasing without hiding a real change. Defaults to 0."),
		"ignore_regions": map[string]any{
			"type":        "array",
			"description": "Named rectangles excluded from the pixel comparison, in CSS pixels. Regions recorded with a baseline keep applying to later checks.",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   stringSchema("What this rectangle covers, for example \"clock\" or \"avatar\". Reported back so you can see which exclusion swallowed a change."),
					"x":      integerSchema("Left edge in CSS pixels."),
					"y":      integerSchema("Top edge in CSS pixels."),
					"width":  integerSchema("Width in CSS pixels."),
					"height": integerSchema("Height in CSS pixels."),
				},
				"required": []string{"name", "x", "y", "width", "height"},
			},
		},
		"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
	}, []string{"action", "recipe_digest", "step_index"}))
}
