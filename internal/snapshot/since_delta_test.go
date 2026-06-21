package snapshot

import (
	"context"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

func sinceVersion(t *testing.T, s PageSnapshot) int64 {
	t.Helper()
	v, ok := s.Metadata["version"].(float64)
	if !ok {
		t.Fatalf("snapshot metadata has no numeric version: %#v", s.Metadata)
	}
	return int64(v)
}

func refByName(els []Element, name string) string {
	for i := range els {
		if els[i].Name == name {
			return els[i].Ref
		}
	}
	return ""
}

func sinceContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func sinceNavigate(t *testing.T, ctx context.Context, html string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}
}

func sinceMutate(t *testing.T, ctx context.Context, js string) {
	t.Helper()
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("(function(){"+js+"; return true;})()", &ok)); err != nil {
		t.Fatalf("mutate: %v", err)
	}
}

// TestSinceDeltaReportsAddedChangedRemoved is the core delta path: after a
// baseline snapshot, one element is changed, one removed from the DOM, and one
// added; a since-snapshot must report exactly those, and 'elements' must carry
// only the added+changed elements (the unchanged one is omitted).
func TestSinceDeltaReportsAddedChangedRemoved(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body>
<button id="b">Alpha</button>
<a id="lnk" href="/x">Link</a>
<input id="inp" aria-label="Field">
</body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	if v1.Delta != nil {
		t.Fatalf("baseline (no since) must not carry a delta")
	}
	btnRef := refByName(v1.Elements, "Alpha")
	linkRef := refByName(v1.Elements, "Link")
	if btnRef == "" || linkRef == "" {
		t.Fatalf("missing baseline refs: btn=%q link=%q\n%+v", btnRef, linkRef, v1.Elements)
	}

	// Change the button's name, remove the link from the DOM, add a new button.
	sinceMutate(t, ctx, `document.getElementById('b').textContent='Beta';
document.getElementById('lnk').remove();
var n=document.createElement('button'); n.textContent='Gamma'; document.body.appendChild(n);`)

	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, v1)})
	if err != nil {
		t.Fatalf("delta snapshot: %v", err)
	}
	if v2.Delta == nil {
		t.Fatalf("expected a delta when since matches and options are identical")
	}
	if !sinceContains(v2.Delta.Changed, btnRef) {
		t.Fatalf("button rename not reported as changed: %+v", v2.Delta)
	}
	if !sinceContains(v2.Delta.Removed, linkRef) {
		t.Fatalf("removed link not reported as removed: %+v", v2.Delta)
	}
	if len(v2.Delta.Added) == 0 {
		t.Fatalf("added button not reported: %+v", v2.Delta)
	}
	// elements is a change set: the unchanged input must NOT be present, but the
	// changed button must be.
	if refByName(v2.Elements, "Field") != "" {
		t.Fatalf("unchanged element leaked into delta elements: %+v", v2.Elements)
	}
	if refByName(v2.Elements, "Beta") == "" {
		t.Fatalf("changed element missing from delta elements: %+v", v2.Elements)
	}
}

// TestSinceDeltaFullWhenSinceAbsentOrMismatch guards backward compatibility: a
// snapshot with no since (or a non-matching since) is a full snapshot with no
// delta, byte-equivalent to pre-feature behaviour for existing callers.
func TestSinceDeltaFullWhenSinceAbsentOrMismatch(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body><button>Alpha</button></body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if v1.Delta != nil {
		t.Fatalf("snapshot without since must have no delta")
	}
	// A stale/unknown since version falls back to a full snapshot.
	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Since: 999999})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Delta != nil {
		t.Fatalf("non-matching since must fall back to a full snapshot, got delta %+v", v2.Delta)
	}
	if len(v2.Elements) == 0 {
		t.Fatalf("full fallback returned no elements")
	}
}

// TestSinceDeltaOptionsMismatchReturnsFull guards finding #7: the version cache
// is keyed by the option envelope, so a since-snapshot taken with DIFFERENT
// options (here a different query) than the cached version must return a full
// snapshot, never a delta computed across unrelated element sets.
func TestSinceDeltaOptionsMismatchReturnsFull(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body>
<button>Alpha</button><button>Beta</button>
</body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Query: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	// Same version, but different options -> must NOT produce a delta.
	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Query: "Beta", Since: sinceVersion(t, v1)})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Delta != nil {
		t.Fatalf("since with changed options must return a full snapshot, got delta %+v", v2.Delta)
	}
	if refByName(v2.Elements, "Beta") == "" {
		t.Fatalf("full snapshot for the new query missing its element: %+v", v2.Elements)
	}
}

// TestSinceDeltaDetectsCheckboxStateChange guards that the fingerprint covers the
// COMPLETE emitted payload, not a hand-picked subset: toggling a checkbox changes
// `checked` (and aria state), which must be reported as a changed element even
// though role/name/value/href are unchanged.
func TestSinceDeltaDetectsCheckboxStateChange(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body>
<input type="checkbox" id="cb" aria-label="Agree">
</body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	cbRef := refByName(v1.Elements, "Agree")
	if cbRef == "" {
		t.Fatalf("baseline missing checkbox: %+v", v1.Elements)
	}

	// Toggle checked (a state-only change: role/name/value/href all stay the same).
	sinceMutate(t, ctx, `document.getElementById('cb').checked = true;`)

	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, v1)})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Delta == nil {
		t.Fatalf("expected a delta")
	}
	if !sinceContains(v2.Delta.Changed, cbRef) {
		t.Fatalf("checkbox state toggle not reported as changed: %+v (elements=%+v)", v2.Delta, v2.Elements)
	}
	if refByName(v2.Elements, "Agree") == "" {
		t.Fatalf("changed checkbox missing from delta elements: %+v", v2.Elements)
	}
}

// TestSinceDeltaVisualIslandsLimitMismatchReturnsFull guards that
// visual_islands_limit is part of the option-envelope key, so changing it between
// snapshots forces a full snapshot rather than a cross-envelope delta.
func TestSinceDeltaVisualIslandsLimitMismatchReturnsFull(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body><button>Alpha</button></body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", VisualIslands: true, VisualIslandsLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", VisualIslands: true, VisualIslandsLimit: 10, Since: sinceVersion(t, v1)})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Delta != nil {
		t.Fatalf("changing visual_islands_limit must fall back to a full snapshot, got delta %+v", v2.Delta)
	}
}

// TestSinceDeltaDoesNotFalselyReportFilteredOutRefAsRemoved guards finding #6:
// 'removed' is computed from DOM presence, not the filtered returned set. A ref
// that stops MATCHING the (unchanged) query but stays in the DOM must NOT be
// reported as removed.
func TestSinceDeltaDoesNotFalselyReportFilteredOutRefAsRemoved(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<!DOCTYPE html><html><body>
<button>Save Alpha</button><button>Save Beta</button><button>Other</button>
</body></html>`)

	v1, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Query: "Save"})
	if err != nil {
		t.Fatal(err)
	}
	saveAlphaRef := refByName(v1.Elements, "Save Alpha")
	if saveAlphaRef == "" {
		t.Fatalf("baseline missing 'Save Alpha': %+v", v1.Elements)
	}

	// Rename "Save Alpha" so it no longer matches the query, but keep it in the DOM.
	sinceMutate(t, ctx, `for (const b of document.querySelectorAll('button')) { if (b.textContent==='Save Alpha') b.textContent='Renamed Alpha'; }`)

	v2, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Query: "Save", Since: sinceVersion(t, v1)})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Delta == nil {
		t.Fatalf("expected a delta (same query + matching version)")
	}
	if sinceContains(v2.Delta.Removed, saveAlphaRef) {
		t.Fatalf("a ref that left the FILTERED set but is still in the DOM was falsely reported removed: %+v", v2.Delta)
	}
}
