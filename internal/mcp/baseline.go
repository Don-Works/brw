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
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/snapshot"
)

// errBaselinesDisabled is the honest answer from a daemon with no baseline
// root. Silently keeping baselines in a temporary directory would make a gate
// that passes on every fresh process, which is worse than not having one.
var errBaselinesDisabled = errors.New("regression baselines are not enabled on this daemon; start brwd with --baseline-root pointing at a directory outside any repository")

// SetBaselineStore installs the local regression-baseline store.
func (s *Server) SetBaselineStore(store *baseline.Store) { s.baselines = store }

// SetRecipeBaselines installs the private provider's baseline side.
//
// A baseline of a page a private recipe navigated to is a screenshot of private
// content, and the provider already holds the recipe that reached it. When the
// provider owns the recipe, that is where its baselines go; the local root is
// what is left for public fixtures. Nil means no provider implements the
// capability, and everything goes to the local root as before.
func (s *Server) SetRecipeBaselines(store recipe.BaselineStore) {
	s.recipeBaselines = store
	s.baselineRouter = store
}

// SetBaselineRouter installs the routing question WITHOUT a place to store the
// answer. That is a daemon proxying its browser host: the private provider is
// upstream, so this process can ask what the provider owns and cannot write to
// it. Without the question a proxy routes every baseline to its own local root,
// including captures of pages the provider's recipes reach.
func (s *Server) SetBaselineRouter(router recipe.BaselineRouter) { s.baselineRouter = router }

// errBaselineProviderIsUpstream is the refusal on a proxying daemon. Named
// rather than a silent fall back to the local root, and narrower than refusing
// the tool outright: a public fixture's baseline still works here.
var errBaselineProviderIsUpstream = errors.New("this baseline belongs with the private recipe provider, which is on the browser-host daemon this one proxies over --upstream-http; run brw_baseline there rather than writing the capture to this daemon's --baseline-root")

// baselineStorage picks the destination for one capture.
//
// Two questions decide it, and neither is asked of the caller. The provider is
// asked whether it owns the recipe the digest pins, because which store a
// capture belongs in is a property of the recipe. It is asked about the page as
// well, because the digest is an argument an agent invents: an agent sitting on
// a page a private recipe reached can name any well-formed digest the provider
// does not own, and routing on the digest alone would then write that page's
// screenshot and accessible names to the local root. When the provider has a
// recipe for the page's origin and does not own the digest, brw refuses instead
// of guessing.
//
// A provider that errors on either question is NOT quietly treated as "not
// mine". That fallback would put a private-page screenshot in the local root
// exactly when the provider is unreachable, which is the outcome this whole
// path exists to prevent.
func (s *Server) baselineStorage(ctx context.Context, digest, pageURL string) (baseline.Storage, error) {
	if s.baselineRouter != nil {
		route, err := s.baselineRouter.RouteBaseline(ctx, digest, pageURL)
		if err != nil {
			return nil, fmt.Errorf("resolve where this baseline belongs: %w", err)
		}
		if route.OwnsRecipe {
			if s.recipeBaselines == nil {
				return nil, errBaselineProviderIsUpstream
			}
			return recipe.ProviderBaselines(ctx, s.recipeBaselines), nil
		}
		if route.OwnsOrigin {
			return nil, fmt.Errorf("the private recipe provider has a recipe for the page this tab is showing, and recipe_digest %s is not one of its recipes: a capture of that page is not written to the local baseline root, so gate it with the digest of the provider's own recipe for this page", digest)
		}
	}
	if s.baselines == nil {
		return nil, errBaselinesDisabled
	}
	return s.baselines, nil
}

type baselineRequest struct {
	Action           string                  `json:"action"`
	RecipeDigest     string                  `json:"recipe_digest"`
	StepIndex        int                     `json:"step_index"`
	PixelTolerance   float64                 `json:"pixel_tolerance,omitempty"`
	ChannelTolerance int                     `json:"channel_tolerance,omitempty"`
	IgnoreRegions    []baseline.IgnoreRegion `json:"ignore_regions,omitempty"`
	TabID            string                  `json:"tab_id,omitempty"`
}

// baselineCheckResult is a check verdict plus where the baseline it compared
// against lives. The verdict is embedded rather than copied field by field, so
// a field added to baseline.CheckResult cannot be dropped here silently.
type baselineCheckResult struct {
	baseline.CheckResult
	StoredIn string `json:"stored_in,omitempty"`
}

// baselineListResult answers "what do I already have for this step, and under
// what conditions was it taken".
type baselineListResult struct {
	Action       string                 `json:"action"`
	RecipeDigest string                 `json:"recipe_digest"`
	StepIndex    int                    `json:"step_index"`
	Environments []baseline.Environment `json:"environments"`
	Count        int                    `json:"count"`
	// StoredIn names the destination these came from, because there are now two
	// and an operator looking for a file needs to know which.
	StoredIn string `json:"stored_in,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (s *Server) callBaseline(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	if s.baselines == nil && s.baselineRouter == nil {
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

	// The action is checked before the destination is resolved: routing asks the
	// provider a question over the network, and a typo in the action is not a
	// reason to ask it.
	switch action {
	case "check", "update", "list", "delete":
	case "":
		return toolError(errors.New(`action is required: "check", "update", "list", or "delete"`)), nil
	default:
		return toolError(fmt.Errorf("unknown action %q: want check, update, list, or delete", req.Action)), nil
	}
	// Checked here too, although every store checks it again: routing sends the
	// digest to a third party, and a malformed one should be refused by brw
	// rather than forwarded to the operator's provider.
	digest, err := baseline.NormalizeRecipeDigest(req.RecipeDigest)
	if err != nil {
		return toolError(err), nil
	}
	// The page is half of the routing question, so it is resolved before the
	// destination is. Only the actions that capture the page need it: list reads
	// no page and writes nothing, and requiring a live tab for it would refuse a
	// call that has nothing to leak.
	var pageURL string
	if s.baselineRouter != nil && (action == "check" || action == "update") {
		pageURL, err = s.currentPageOrigin(ctx, req.TabID)
		if err != nil {
			return toolError(fmt.Errorf("a baseline is routed by the page it captures as well as by the digest, and this tab's URL could not be read: %w", err)), nil
		}
	}
	store, err := s.baselineStorage(ctx, digest, pageURL)
	if err != nil {
		return toolError(err), nil
	}

	switch action {
	case "list":
		environments, err := store.EnvironmentsFor(req.RecipeDigest, req.StepIndex)
		if err != nil {
			return toolError(err), nil
		}
		if environments == nil {
			environments = []baseline.Environment{}
		}
		result := baselineListResult{
			Action: action, RecipeDigest: req.RecipeDigest, StepIndex: req.StepIndex,
			Environments: environments, Count: len(environments), StoredIn: store.Location(),
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
		// Both transports capture the viewport as JPEG for wire size. A baseline
		// is stored and compared as PNG, so the encoding is normalized once here
		// rather than leaving half the store holding JPEG bytes in a .png file.
		pixels, err = baseline.NormalizePNG(pixels)
		if err != nil {
			return toolError(err), nil
		}
		tree, err := s.ariaTree(ctx)
		if err != nil {
			return toolError(err), nil
		}
		result, err := baseline.Check(store, baseline.CheckOptions{
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
		return toolJSON(baselineCheckResult{CheckResult: result, StoredIn: store.Location()}, nil)

	case "delete":
		environment, err := s.baselineEnvironment(ctx)
		if err != nil {
			return toolError(err), nil
		}
		key := baseline.Key{RecipeDigest: req.RecipeDigest, StepIndex: req.StepIndex, Environment: environment}
		if err := store.Delete(key); err != nil {
			return toolError(err), nil
		}
		return toolJSON(map[string]any{
			"action": action, "baseline_id": key.ID(), "stored_in": store.Location(),
			"note": "baseline deleted",
		}, nil)
	}
	// Unreachable: the action was validated above. Kept as a refusal rather than
	// a panic so a new action added to one switch and not the other is answered.
	return toolError(fmt.Errorf("unknown action %q: want check, update, list, or delete", req.Action)), nil
}

func (s *Server) baselineEnvironment(ctx context.Context) (baseline.Environment, error) {
	raw, err := s.manager.Evaluate(ctx, baseline.EnvironmentExpression)
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
// update only happens on the update action, nothing is written by a check, and
// the destination is resolved from the recipe AND the page in baselineStorage.
func baselineTool() map[string]any {
	return tool("brw_baseline", "Gate a page against a stored regression baseline instead of eyeballing a screenshot. Actions: check (compare the current page against the baseline for this recipe step and environment — WRITES NOTHING, ever, including when it passes), update (replace that baseline with what the page looks like now; this is the only way a baseline changes), list (the environments a baseline already exists under for this step), delete (drop one baseline). A baseline is keyed by recipe_digest + step_index + an environment fingerprint brw measures itself: browser build, viewport, device pixel ratio, locale and the browser host's OS. If a baseline exists for the step but under different conditions, check reports status \"environment_mismatch\" and names the fields that moved (\"device_pixel_ratio 1 -> 2\") instead of a screen full of false pixel differences. Two comparisons run. The visual one counts pixels that moved, with pixel_tolerance as the fraction of compared pixels allowed to differ, channel_tolerance as the per-channel slack that absorbs anti-aliasing, and ignore_regions as NAMED rectangles in CSS pixels — put the clock, the avatar and the ad slot in there or every run fails. The structural one diffs the page's ARIA structure, role and accessible name, which catches a button losing its label — invisible to a pixel diff. It carries no geometry, so a change that only repaints (a font bump, a colour) moves the visual half and leaves the structural half alone. status is \"match\", \"diff\", \"environment_mismatch\", \"missing\" (nothing stored yet; run update once to record it), \"recorded\" or \"updated\"; failed is the single boolean to branch on. Baselines are stored by the daemon that runs the check, and stored_in names which of its two destinations was used: a recipe the private provider owns keeps its baselines with that provider, because the capture is of a page that recipe reached; everything else goes to the operator-configured local root, which the daemon refuses to place inside a Git working tree — so a baseline of a signed-in page cannot end up committed. You do not choose the destination and there is no argument for it: the recipe and the PAGE decide. recipe_digest is an argument you supply, so it is not trusted on its own — if the provider has a recipe for the origin the tab is showing and the digest you passed is not one of its recipes, the call is refused instead of writing that page's capture locally. A daemon whose provider cannot be reached fails the call rather than writing that capture to the local root, and one that proxies its browser host (--upstream-http) refuses by name the baselines that belong with the provider, because the provider is on the host, not here.", object(map[string]any{
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
			"description": "Named rectangles excluded from the pixel comparison, in CSS pixels — the page's own coordinates, as you would read them off the layout. The capture itself is downscaled, and brw places each rectangle from the capture's width against the viewport in the key, so you never convert. A region that lands off the capture excluded nothing and is reported under regions_outside_capture instead of ignored_regions. Regions recorded with a baseline keep applying to later checks.",
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
