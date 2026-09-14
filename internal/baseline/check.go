package baseline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// Statuses a baseline check reports.
const (
	// StatusMissing means nothing is stored for this key and none of the other
	// stored environments explains why. The check fails: a gate that passes
	// because it has never seen the page is not a gate.
	StatusMissing = "missing"
	// StatusEnvironmentMismatch means a baseline exists for this recipe step,
	// but under different capture conditions. The pixels are not compared,
	// because the diff would be real and meaningless.
	StatusEnvironmentMismatch = "environment_mismatch"
	StatusMatch               = "match"
	StatusDiff                = "diff"
	// StatusRecorded is the first capture for a key, and StatusUpdated is an
	// accepted change. Both require update:true.
	StatusRecorded = "recorded"
	StatusUpdated  = "updated"
)

// CheckOptions is one comparison.
type CheckOptions struct {
	Key           Key
	Screenshot    []byte
	Tree          snapshot.AriaTree
	IgnoreRegions []IgnoreRegion
	// PixelTolerance and ChannelTolerance are the visual slack; see
	// VisualOptions.
	PixelTolerance   float64
	ChannelTolerance uint8
	// Update is the explicit acceptance. Without it Check never writes, which
	// is the property that stops a baseline from quietly absorbing the
	// regression it exists to catch.
	Update bool
	Now    func() time.Time
}

// EnvironmentMismatch reports one stored environment and how it differs from
// the one being checked.
type EnvironmentMismatch struct {
	Stored      Environment `json:"stored"`
	Differences []string    `json:"differences"`
}

// CheckResult is the verdict.
type CheckResult struct {
	Status      string      `json:"status"`
	BaselineID  string      `json:"baseline_id"`
	Environment Environment `json:"environment"`
	// Failed is the single boolean a caller gates on, so nobody has to
	// enumerate which statuses mean "stop".
	Failed              bool                  `json:"failed"`
	EnvironmentMismatch []EnvironmentMismatch `json:"environment_mismatch,omitempty"`
	Visual              *VisualDiff           `json:"visual,omitempty"`
	ARIA                *snapshot.AriaDiff    `json:"aria,omitempty"`
	BaselineCreatedAt   *time.Time            `json:"baseline_created_at,omitempty"`
	IgnoredRegions      []string              `json:"ignored_regions,omitempty"`
	Note                string                `json:"note,omitempty"`
}

// Check compares one capture against its baseline.
//
// The order matters. An unknown key is answered from the OTHER environments
// stored for the same recipe step before anything is compared, so a run on a
// retina display against a baseline captured at 1x reports the display, not a
// screen full of moved pixels.
func Check(store *Store, opts CheckOptions) (CheckResult, error) {
	if store == nil {
		return CheckResult{}, errors.New("no baseline store is configured on this daemon")
	}
	if err := opts.Key.Validate(); err != nil {
		return CheckResult{}, err
	}
	if len(opts.Screenshot) == 0 {
		return CheckResult{}, errors.New("a baseline check needs a screenshot of the current page")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	key := opts.Key
	key.Environment = key.Environment.Normalize()
	result := CheckResult{BaselineID: key.ID(), Environment: key.Environment}

	stored, found, err := store.Load(key)
	if err != nil {
		return CheckResult{}, err
	}
	if !found {
		others, err := store.EnvironmentsFor(key.RecipeDigest, key.StepIndex)
		if err != nil {
			return CheckResult{}, err
		}
		for _, other := range others {
			result.EnvironmentMismatch = append(result.EnvironmentMismatch, EnvironmentMismatch{
				Stored:      other,
				Differences: other.Differences(key.Environment),
			})
		}
		if opts.Update {
			if err := writeBaseline(store, key, opts, now); err != nil {
				return CheckResult{}, err
			}
			result.Status = StatusRecorded
			result.Note = "recorded a new baseline for this environment; nothing was compared"
			return result, nil
		}
		if len(result.EnvironmentMismatch) > 0 {
			result.Status = StatusEnvironmentMismatch
			result.Failed = true
			result.Note = fmt.Sprintf("a baseline exists for this recipe step but under %d other environment(s): %s — this is an environment difference, not a visual regression; re-run under the recorded environment, or record this one with update",
				len(result.EnvironmentMismatch), strings.Join(mismatchSummary(result.EnvironmentMismatch), "; "))
			return result, nil
		}
		result.Status = StatusMissing
		result.Failed = true
		result.Note = "no baseline is stored for this key; re-run with update to record one"
		return result, nil
	}

	regions := unionRegions(stored.IgnoreRegions, opts.IgnoreRegions)
	visual, err := CompareImages(stored.Screenshot, opts.Screenshot, VisualOptions{
		PixelTolerance:   opts.PixelTolerance,
		ChannelTolerance: opts.ChannelTolerance,
		IgnoreRegions:    regions,
		DevicePixelRatio: key.Environment.DevicePixelRatio,
	})
	if err != nil {
		return CheckResult{}, err
	}
	aria := snapshot.DiffAriaTrees(stored.Tree, opts.Tree)
	created := stored.CreatedAt
	result.Visual = &visual
	result.ARIA = &aria
	result.BaselineCreatedAt = &created
	result.IgnoredRegions = visual.IgnoredRegions

	if opts.Update {
		if err := writeBaseline(store, key, CheckOptions{
			Key:           key,
			Screenshot:    opts.Screenshot,
			Tree:          opts.Tree,
			IgnoreRegions: regions,
		}, now); err != nil {
			return CheckResult{}, err
		}
		result.Status = StatusUpdated
		result.Note = "baseline replaced on explicit update; the differences above are what was accepted"
		return result, nil
	}
	if visual.Changed || aria.Changed {
		result.Status = StatusDiff
		result.Failed = true
		result.Note = diffNote(visual, aria)
		return result, nil
	}
	result.Status = StatusMatch
	return result, nil
}

func writeBaseline(store *Store, key Key, opts CheckOptions, now func() time.Time) error {
	return store.Save(Record{
		Key:           key,
		Environment:   key.Environment,
		Tree:          opts.Tree,
		IgnoreRegions: opts.IgnoreRegions,
		CreatedAt:     now().UTC(),
		Screenshot:    opts.Screenshot,
	})
}

func mismatchSummary(mismatches []EnvironmentMismatch) []string {
	out := make([]string, 0, len(mismatches))
	for _, mismatch := range mismatches {
		if len(mismatch.Differences) == 0 {
			continue
		}
		out = append(out, strings.Join(mismatch.Differences, ", "))
	}
	return out
}

// diffNote says which of the two checks failed, because they mean different
// things: pixels moved is a rendering change, and the ARIA structure moving is
// a semantic one that a pixel diff can miss entirely.
func diffNote(visual VisualDiff, aria snapshot.AriaDiff) string {
	switch {
	case visual.Changed && aria.Changed:
		return fmt.Sprintf("%d pixels moved and the ARIA structure changed in %d place(s)", visual.DiffPixels, aria.Count)
	case visual.Changed:
		return fmt.Sprintf("%d of %d compared pixels moved, over the %g tolerance; the ARIA structure is unchanged", visual.DiffPixels, visual.ComparedPixels, visual.Tolerance)
	default:
		return fmt.Sprintf("the pixels are within tolerance but the ARIA structure changed in %d place(s) — a role or accessible name moved, which a pixel diff does not see", aria.Count)
	}
}

// unionRegions keeps the regions recorded with the baseline as well as any the
// current check names. A caller that forgets to pass the clock's rectangle
// should not be told the clock is a regression.
func unionRegions(stored, requested []IgnoreRegion) []IgnoreRegion {
	seen := map[IgnoreRegion]bool{}
	out := make([]IgnoreRegion, 0, len(stored)+len(requested))
	for _, region := range append(append([]IgnoreRegion{}, stored...), requested...) {
		if !region.valid() || seen[region] {
			continue
		}
		seen[region] = true
		out = append(out, region)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}
