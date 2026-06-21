package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// metaVersion extracts metadata.version as an int64 (JSON numbers decode to
// float64 into a map[string]interface{}).
func metaVersion(t *testing.T, snap PageSnapshot) int64 {
	t.Helper()
	v, ok := snap.Metadata["version"]
	if !ok {
		t.Fatalf("snapshot metadata missing version: %+v", snap.Metadata)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("metadata.version is %T, want number", v)
	}
	return int64(f)
}

func refSet(els []Element) map[string]Element {
	out := make(map[string]Element, len(els))
	for i := range els {
		out[els[i].Ref] = els[i]
	}
	return out
}

// findByName returns the first element with the given Name, or nil. (The external
// snapshot_test package has its own copy; this is the in-package equivalent.)
func findByName(els []Element, name string) *Element {
	for i := range els {
		if els[i].Name == name {
			return &els[i]
		}
	}
	return nil
}

// TestSinceDeltaReportsOnlyChangedElement proves ITEM A: when Since names a
// cached prior version, the snapshot returns ONLY the elements that changed
// (added/changed bodies + removed refs) plus the advanced version, instead of the
// full element set. It also proves the backward-compat contract: Since=0 returns
// the full set with no delta attached.
func TestSinceDeltaReportsOnlyChangedElement(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<button id="alpha">Alpha</button>
<button id="beta">Beta</button>
<button id="gamma">Gamma</button>
<a id="lnk" href="/x">Link</a>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	// 1. Baseline full snapshot (mode all so non-viewport defaults don't drop
	//    elements). This stamps data-brw-ref on every element and seeds the
	//    in-page version cache.
	base, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	if base.Delta != nil {
		t.Fatalf("baseline snapshot unexpectedly carried a delta: %+v", base.Delta)
	}
	if len(base.Elements) < 4 {
		t.Fatalf("baseline returned %d elements, want >=4 (a,b,c buttons + link): %v",
			len(base.Elements), names(base.Elements))
	}
	baseV := metaVersion(t, base)
	baseRefs := refSet(base.Elements)

	// Find the ref of the "Beta" button so we can assert exactly it changes.
	var betaRef string
	for _, el := range base.Elements {
		if el.Name == "Beta" {
			betaRef = el.Ref
		}
	}
	if betaRef == "" {
		t.Fatalf("baseline did not surface the Beta button: %v", names(base.Elements))
	}

	// 2. Mutate exactly one element's visible text (changes its name fingerprint
	//    while preserving its stamped data-brw-ref, so the same ref reports as
	//    "changed" rather than added/removed).
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.getElementById('beta').textContent = 'Beta CHANGED';`, nil)); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	// 3. Delta snapshot against the baseline version: ONLY the changed element is
	//    reported, the version advanced, and the delta change set names exactly it.
	delta, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Since: baseV})
	if err != nil {
		t.Fatalf("delta snapshot: %v", err)
	}
	if delta.Delta == nil {
		t.Fatalf("expected a delta for Since=%d (cache hit), got full snapshot with %d elements",
			baseV, len(delta.Elements))
	}
	if delta.Delta.Since != baseV {
		t.Fatalf("delta.since = %d, want %d", delta.Delta.Since, baseV)
	}
	deltaV := metaVersion(t, delta)
	if deltaV <= baseV {
		t.Fatalf("version did not advance: delta version %d <= base %d", deltaV, baseV)
	}
	if delta.Delta.Version != deltaV {
		t.Fatalf("delta.version %d != metadata.version %d", delta.Delta.Version, deltaV)
	}

	// Exactly one changed element, no adds, no removes.
	if len(delta.Delta.Added) != 0 {
		t.Fatalf("unexpected added refs: %v", delta.Delta.Added)
	}
	if len(delta.Delta.Removed) != 0 {
		t.Fatalf("unexpected removed refs: %v", delta.Delta.Removed)
	}
	if len(delta.Delta.Changed) != 1 || delta.Delta.Changed[0] != betaRef {
		t.Fatalf("changed refs = %v, want exactly [%s]", delta.Delta.Changed, betaRef)
	}

	// The returned element body set must be ONLY the changed element.
	if len(delta.Elements) != 1 {
		t.Fatalf("delta returned %d element bodies, want exactly 1 (the changed Beta): %v",
			len(delta.Elements), names(delta.Elements))
	}
	if delta.Elements[0].Ref != betaRef {
		t.Fatalf("delta element ref = %s, want %s", delta.Elements[0].Ref, betaRef)
	}
	if delta.Elements[0].Name != "Beta CHANGED" {
		t.Fatalf("delta element name = %q, want %q", delta.Elements[0].Name, "Beta CHANGED")
	}
	// Source defaulting (Go-side normalization) must still apply on the delta path.
	if len(delta.Elements[0].Source) == 0 {
		t.Fatalf("delta element missing source: %+v", delta.Elements[0])
	}

	// 4. Backward-compat: Since=0 still returns the FULL set with no delta, and the
	//    full set reflects the mutation (Beta now reads "Beta CHANGED").
	full, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("full snapshot after mutation: %v", err)
	}
	if full.Delta != nil {
		t.Fatalf("Since=0 snapshot unexpectedly carried a delta: %+v", full.Delta)
	}
	if len(full.Elements) != len(base.Elements) {
		t.Fatalf("full element count %d != baseline %d", len(full.Elements), len(base.Elements))
	}
	fullRefs := refSet(full.Elements)
	for ref := range baseRefs {
		if _, ok := fullRefs[ref]; !ok {
			t.Fatalf("full snapshot dropped ref %s present at baseline", ref)
		}
	}
	if fullRefs[betaRef].Name != "Beta CHANGED" {
		t.Fatalf("full snapshot Beta name = %q, want %q", fullRefs[betaRef].Name, "Beta CHANGED")
	}
}

// TestSinceDeltaCacheMissFallsBackToFull proves the cache-miss path (e.g. after a
// navigation replaces the JS context, so the version cache is gone) returns the
// FULL snapshot even when Since is set, preserving backward compatibility.
func TestSinceDeltaCacheMissFallsBackToFull(t *testing.T) {
	page1 := `<!DOCTYPE html><html><body><button id="a">Alpha</button><button id="b">Beta</button></body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(page1))); err != nil {
		t.Fatal(err)
	}

	base, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	baseV := metaVersion(t, base)

	// Navigate to a fresh document: the in-page version cache (window.__brw) is
	// wiped with the JS context. A Since referencing the old version can no longer
	// be a cache hit.
	page2 := `<!DOCTYPE html><html><body><button id="c">Gamma</button></body></html>`
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(page2))); err != nil {
		t.Fatal(err)
	}

	after, err := EvaluateWithOptions(ctx, SnapshotOptions{Mode: "all", Since: baseV})
	if err != nil {
		t.Fatalf("post-nav snapshot with stale Since: %v", err)
	}
	if after.Delta != nil {
		t.Fatalf("post-nav stale Since should fall back to full snapshot, got delta: %+v", after.Delta)
	}
	if len(after.Elements) == 0 {
		t.Fatalf("post-nav full snapshot returned no elements")
	}
	if findByName(after.Elements, "Gamma") == nil {
		t.Fatalf("post-nav full snapshot missing new-page element Gamma: %v", names(after.Elements))
	}
}
