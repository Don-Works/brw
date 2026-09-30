package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

func historySnapshot(t *testing.T, ctx context.Context, opts SnapshotOptions) PageSnapshot {
	t.Helper()
	snap, err := EvaluateWithOptions(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func historyFallback(t *testing.T, snap PageSnapshot, reason string) {
	t.Helper()
	if snap.Delta != nil || snap.Metadata["delta"] != false || snap.Metadata["delta_fallback"] != reason {
		t.Fatalf("expected full snapshot with %q fallback, got delta=%+v metadata=%+v", reason, snap.Delta, snap.Metadata)
	}
}

func historyStats(t *testing.T, ctx context.Context) (int, int) {
	t.Helper()
	var got struct {
		Count  int `json:"count"`
		Bytes  int `json:"bytes"`
		Actual int `json:"actual"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){const h=window.__brw.deltaHistory;return {count:h.entries.length,bytes:h.bytes,actual:h.entries.reduce((n,e)=>n+new TextEncoder().encode(e.payload).length,0)};})()`, &got)); err != nil {
		t.Fatal(err)
	}
	if got.Bytes != got.Actual || got.Bytes > 2<<20 || got.Count > 8 {
		t.Fatalf("invalid history bounds/accounting: %+v", got)
	}
	return got.Count, got.Bytes
}

func TestDeltaHistoryInterleavedFindAndObservation(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button id="a">Alpha</button><button id="b">Beta</button><button id="c">Keep</button>`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	if _, err := Find(ctx, FindOptions{Query: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	historySnapshot(t, ctx, SnapshotOptions{ViewportOnly: true})
	sinceMutate(t, ctx, `document.querySelector('#a').textContent='Changed';document.querySelector('#b').remove();const b=document.createElement('button');b.id='new';b.textContent='New';document.body.append(b)`)
	historySnapshot(t, ctx, SnapshotOptions{ViewportOnly: true})
	delta := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)})
	if delta.Delta == nil || len(delta.Delta.Added) != 1 || len(delta.Delta.Changed) != 1 || len(delta.Delta.Removed) != 1 || len(delta.Elements) != 2 {
		t.Fatalf("wrong interleaved delta: %+v", delta)
	}
	if !sinceContains(delta.Delta.Changed, refByName(base.Elements, "Alpha")) || !sinceContains(delta.Delta.Removed, refByName(base.Elements, "Beta")) || refByName(delta.Elements, "Keep") != "" {
		t.Fatalf("wrong refs in delta: %+v", delta)
	}
}

func TestDeltaHistoryChainedDeltasRetainCompleteViews(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button id="a">Alpha</button><button id="b">Beta</button>`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	sinceMutate(t, ctx, `document.querySelector('#a').textContent='Alpha changed'`)
	middle := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)})
	if middle.Delta == nil || len(middle.Elements) != 1 {
		t.Fatalf("bad first delta: %+v", middle)
	}
	sinceMutate(t, ctx, `document.querySelector('#b').textContent='Beta changed'`)
	last := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, middle)})
	if last.Delta == nil || len(last.Delta.Added) != 0 || len(last.Delta.Changed) != 1 || len(last.Elements) != 1 || last.Elements[0].Name != "Beta changed" {
		t.Fatalf("partial response corrupted retained baseline: %+v", last)
	}
}

func TestDeltaHistoryLookupBeforeCountEviction(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button id="a">Alpha</button>`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	for i := 0; i < 7; i++ {
		historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	}
	if count, _ := historyStats(t, ctx); count != 8 {
		t.Fatalf("count=%d", count)
	}
	last := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)})
	if last.Delta == nil || len(last.Elements) != 0 {
		t.Fatalf("baseline evicted before lookup: %+v", last)
	}
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)}), "baseline_missing")
}

func TestDeltaHistoryUTF8BudgetAndOversizedBaseline(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<textarea id="a" aria-label="Field"></textarea>`)
	sinceMutate(t, ctx, `document.querySelector('#a').value='😀'.repeat(200000)`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	count, bytes := historyStats(t, ctx)
	if count != 2 || bytes < 1500000 {
		t.Fatalf("UTF-8 budget not exercised: count=%d bytes=%d", count, bytes)
	}
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)}), "baseline_missing")
	sinceMutate(t, ctx, `document.querySelector('#a').value='😀'.repeat(600000)`)
	oversized := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	historyStats(t, ctx)
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, oversized)}), "baseline_missing")
}

func TestDeltaHistoryNavigationAndEpochReset(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button>Old</button>`)
	old := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	sinceNavigate(t, ctx, `<button>New</button>`)
	fresh := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	if sinceVersion(t, fresh) <= sinceVersion(t, old) {
		t.Fatal("versions restarted on navigation")
	}
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, old)}), "baseline_missing")
	sinceMutate(t, ctx, `window.__brw.deltaHistory.epoch='previous-daemon'`)
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, fresh)}), "baseline_missing")
	if count, _ := historyStats(t, ctx); count != 1 {
		t.Fatalf("epoch did not reset history: %d", count)
	}
}

func TestDeltaHistoryOptionsTupleAndEnrichment(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button>Alpha</button>`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Query: "a\x01b", Text: "c"})
	historyFallback(t, historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Query: "a", Text: "b\x01c", Since: sinceVersion(t, base)}), "options_changed")
	base = historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	formatted := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Format: "compact", Since: sinceVersion(t, base)})
	if formatted.Delta == nil {
		t.Fatal("format changed semantic identity")
	}
	for _, opts := range []SnapshotOptions{{Mode: "all", IncludeAX: true}, {Mode: "all", IncludeFrames: true}} {
		opts.Since = sinceVersion(t, base)
		historyFallback(t, historySnapshot(t, ctx, opts), "enriched_snapshot")
	}
}

func TestDeltaHistoryRawWalkDoesNotRetainVersion(t *testing.T) {
	ctx, cancel := structuredTestContext(t)
	defer cancel()
	sinceNavigate(t, ctx, `<button>Alpha</button>`)
	base := historySnapshot(t, ctx, SnapshotOptions{Mode: "all"})
	var raw PageSnapshot
	if err := chromedp.Run(ctx, chromedp.Evaluate(SnapshotScript, &raw)); err != nil {
		t.Fatal(err)
	}
	if sinceVersion(t, raw) != 0 {
		t.Fatalf("raw walk minted retained version: %+v", raw.Metadata)
	}
	if count, _ := historyStats(t, ctx); count != 1 {
		t.Fatalf("raw walk modified history: %d", count)
	}
	args, err := json.Marshal(SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)})
	if err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(SnapshotFunctionScript+"("+string(args)+")", &raw)); err != nil {
		t.Fatal(err)
	}
	historyFallback(t, raw, "untracked_snapshot")
	next := historySnapshot(t, ctx, SnapshotOptions{Mode: "all", Since: sinceVersion(t, base)})
	if next.Delta == nil {
		t.Fatal("raw walk destroyed typed baseline")
	}
}

func TestDeltaHistoryVersionAllocationIsConcurrentAndSafe(t *testing.T) {
	var wg sync.WaitGroup
	versions := make(chan uint64, 128)
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hot, _ := SnapshotCallExpressions(SnapshotOptions{})
			var args struct {
				Version uint64 `json:"__brw_version"`
			}
			if err := json.Unmarshal([]byte(hot[strings.Index(hot, "(")+1:len(hot)-1]), &args); err != nil {
				t.Error(err)
				return
			}
			versions <- args.Version
		}()
	}
	wg.Wait()
	close(versions)
	seen := map[uint64]bool{}
	for version := range versions {
		if version == 0 || version > 9007199254740991 || seen[version] {
			t.Fatalf("unsafe or reused version: %d", version)
		}
		seen[version] = true
	}
	if len(seen) != 128 {
		t.Fatalf("versions=%d", len(seen))
	}
}
