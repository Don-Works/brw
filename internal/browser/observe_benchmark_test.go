package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// observeBenchFixture is a form dense enough that the post-action observation
// carries a full frontier element list, which is what the observe levels trim.
// A three-control page would understate the saving to the point of dishonesty.
func observeBenchFixture() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Ten step form</title></head><body><form>`)
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, `<label for="f%d">Field number %d</label>`+
			`<input id="f%d" name="field_%d" type="text" placeholder="Enter value %d">`, i, i, i, i, i)
	}
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, `<button type="button" id="b%d" onclick="document.title='step %d'">Action number %d</button>`, i, i, i)
	}
	b.WriteString(`</form></body></html>`)
	return b.String()
}

// tenStepFlow is the fixed flow the measurement runs: five fills and five
// clicks, in the order an agent filling a form and confirming it would.
func tenStepFlow(m *Manager, ctx context.Context) []func() (ActionResult, error) {
	steps := make([]func() (ActionResult, error), 0, 10)
	for i := 1; i <= 5; i++ {
		query := fmt.Sprintf("Field number %d", i)
		value := fmt.Sprintf("fixture-value-%d", i)
		steps = append(steps, func() (ActionResult, error) {
			return m.Fill(ctx, snapshot.FillOptions{Query: query, Role: "textbox", Text: value, Replace: true})
		})
	}
	for i := 1; i <= 5; i++ {
		text := fmt.Sprintf("Action number %d", i)
		steps = append(steps, func() (ActionResult, error) {
			return m.ClickText(ctx, snapshot.ClickTextOptions{Text: text, Exact: true})
		})
	}
	return steps
}

// estimatedTokens uses the same 4-chars-per-token estimator as
// scripts/measure-tool-catalogue.py, so the figures in docs/benchmarks.md are
// comparable across the two measurements. It is not a tokenizer.
func estimatedTokens(bytes int) int { return (bytes + 3) / 4 }

// TestObserveLevelsShrinkATenStepFlow is the measurement quoted in
// docs/benchmarks.md. It runs the SAME ten steps against the same real page
// three times and reports what each observe level costs to report. If the levels
// stop trimming, minimal and none stop being smaller and this fails.
func TestObserveLevelsShrinkATenStepFlow(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, observeBenchFixture())
	}))
	defer site.Close()

	measure := func(t *testing.T, level ObserveLevel, explicit bool) int {
		t.Helper()
		m := newHeadlessManager(t)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		opened, err := m.Open(ctx, site.URL)
		if err != nil {
			t.Fatalf("open fixture: %v", err)
		}
		tabCtx := WithTabID(ctx, opened.Tab.ID)

		steps := tenStepFlow(m, tabCtx)
		total := 0
		for i, step := range steps {
			result, err := step()
			if err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			if !result.OK {
				t.Fatalf("step %d not ok: %+v", i, result)
			}
			// The same intermediate/last split a sequence runner applies.
			stepLevel := PlanStepObserveLevel(level, explicit, i, len(steps))
			data, err := json.Marshal(stepLevel.ApplyToAction(result))
			if err != nil {
				t.Fatalf("marshal step %d: %v", i, err)
			}
			total += len(data)
		}
		return total
	}

	full := measure(t, ObserveFull, true)
	defaulted := measure(t, ObserveFull, false)
	minimal := measure(t, ObserveMinimal, true)
	none := measure(t, ObserveNone, true)

	t.Logf("ten-step flow observation cost (bytes of result JSON, ~tokens at 4 chars/token):")
	for _, row := range []struct {
		name  string
		bytes int
	}{
		{"observe=full (today's default on every step)", full},
		{"sequence default (minimal for steps 1-9, full for step 10)", defaulted},
		{"observe=minimal on every step", minimal},
		{"observe=none on every step", none},
	} {
		t.Logf("  %-58s %7d bytes  ~%5d tokens  %5.1f%% of full",
			row.name, row.bytes, estimatedTokens(row.bytes), 100*float64(row.bytes)/float64(full))
	}

	if full <= 0 {
		t.Fatal("the full-level flow measured no bytes at all")
	}
	// A saving that is not measurable is not a saving. These bounds are
	// deliberately loose — the point is that the levels trim, not that a
	// particular page trims by a particular amount — but a level that stopped
	// trimming would sit at 100% and fail them immediately.
	if minimal*5 >= full*4 {
		t.Fatalf("observe=minimal cost %d bytes against full's %d: the element list is not being dropped", minimal, full)
	}
	if none*4 >= minimal*3 {
		t.Fatalf("observe=none cost %d bytes against minimal's %d", none, minimal)
	}
	if defaulted >= full || defaulted <= minimal {
		t.Fatalf("the sequence default cost %d bytes, expected between minimal (%d) and full (%d)", defaulted, minimal, full)
	}
	// none is not free of meaning: every step still reports its outcome.
	if none <= 0 {
		t.Fatal("observe=none reported nothing at all, so a caller cannot tell whether the flow worked")
	}
}
