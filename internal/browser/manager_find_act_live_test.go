package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// findActFixture has one unambiguous control per verb and one deliberately
// ambiguous pair ("Add to cart" / "Add to wishlist") that a substring query for
// "Add" resolves to both.
const findActFixture = `<!doctype html><html><head><meta charset="utf-8"><title>find+act</title></head>
<body>
  <button id="cart" onclick="document.title='cart clicked'">Add to cart</button>
  <button id="wish" onclick="document.title='wish clicked'">Add to wishlist</button>
  <label for="email">Email</label><input id="email" name="email" type="text">
  <label for="size">Size</label>
  <select id="size" name="size"><option value="s">Small</option><option value="l">Large</option></select>
  <button id="only" onclick="document.title='only clicked'">Check out</button>
</body></html>`

func newFindActTab(t *testing.T) (*Manager, context.Context, context.CancelFunc) {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, findActFixture)
	}))
	t.Cleanup(site.Close)

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		cancel()
		t.Fatalf("open fixture: %v", err)
	}
	return m, WithTabID(ctx, opened.Tab.ID), cancel
}

// The whole point of VXD1Q0: locate and act in one call, against a real page,
// with the ambiguity rule enforced on the live search rather than on a fixture
// element list.
func TestFindActLocatesAndActsInOneCall(t *testing.T) {
	tests := []struct {
		name     string
		request  FindAct
		wantRef  bool
		verify   func(t *testing.T, m *Manager, ctx context.Context)
		wantErrs []string
	}{
		{
			name:    "click an unambiguous button",
			request: FindAct{Query: "Check out", Role: "button", Action: "click"},
			wantRef: true,
			verify: func(t *testing.T, m *Manager, ctx context.Context) {
				if got := evalString(t, m, ctx, `document.title`); got != "only clicked" {
					t.Fatalf("title = %q, want %q — find+act did not actuate the button", got, "only clicked")
				}
			},
		},
		{
			name:    "fill a field",
			request: FindAct{Query: "Email", Role: "textbox", Action: "fill", Value: "fixture-user"},
			wantRef: true,
			verify: func(t *testing.T, m *Manager, ctx context.Context) {
				if got := evalString(t, m, ctx, `document.getElementById('email').value`); got != "fixture-user" {
					t.Fatalf("email value = %q, want %q", got, "fixture-user")
				}
			},
		},
		{
			name:    "select an option",
			request: FindAct{Query: "Size", Role: "combobox", Action: "select", Value: "Large"},
			wantRef: true,
			verify: func(t *testing.T, m *Manager, ctx context.Context) {
				if got := evalString(t, m, ctx, `document.getElementById('size').value`); got != "l" {
					t.Fatalf("size value = %q, want %q", got, "l")
				}
			},
		},
		{
			name:     "an ambiguous query errors and clicks nothing",
			request:  FindAct{Query: "Add", Role: "button", Action: "click"},
			wantErrs: []string{"matches 2 elements", "refusing to guess", "Add to cart", "Add to wishlist"},
			verify: func(t *testing.T, m *Manager, ctx context.Context) {
				if got := evalString(t, m, ctx, `document.title`); got != "find+act" {
					t.Fatalf("title = %q, want it untouched — the ambiguous find+act actuated something", got)
				}
			},
		},
		{
			name:    "exact resolves the ambiguity",
			request: FindAct{Query: "Add to cart", Role: "button", Action: "click", Exact: true},
			wantRef: true,
			verify: func(t *testing.T, m *Manager, ctx context.Context) {
				if got := evalString(t, m, ctx, `document.title`); got != "cart clicked" {
					t.Fatalf("title = %q, want %q", got, "cart clicked")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ctx, cancel := newFindActTab(t)
			defer cancel()

			result, err := RunFindAct(ctx, m, tt.request)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("RunFindAct() acted on %s, want an ambiguity error", result.Matched.Ref)
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q does not contain %q", err, want)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("RunFindAct() = %v", err)
				}
				if tt.wantRef && result.Matched.Ref == "" {
					t.Fatal("find+act reported no matched element")
				}
				if !result.Result.OK {
					t.Fatalf("action result not ok: %+v", result.Result)
				}
			}
			tt.verify(t, m, ctx)
		})
	}
}

// A caller cannot narrow its way past the exactly-one rule: the search limit is
// the request's, not the caller's. This is the shape of the wave-2 bypass —
// a guard that an attacker-supplied parameter could satisfy by construction.
func TestFindActIgnoresACallerSuppliedLimit(t *testing.T) {
	m, ctx, cancel := newFindActTab(t)
	defer cancel()

	// A plain find with limit 1 answers with exactly one of the two "Add"
	// buttons: the truncation is what a bypass would lean on.
	truncated, err := m.Find(ctx, snapshot.FindOptions{Query: "Add", Role: "button", Limit: 1})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(truncated.Elements) != 1 {
		t.Fatalf("limit 1 returned %d elements, so this fixture cannot demonstrate the bypass", len(truncated.Elements))
	}

	request := FindAct{Query: "Add", Role: "button", Action: "click"}
	if got := request.FindOptions().Limit; got != FindActResolveLimit {
		t.Fatalf("find+act searched with limit %d, want the fixed %d", got, FindActResolveLimit)
	}
	if _, err := RunFindAct(ctx, m, request); err == nil {
		t.Fatal("find+act resolved an ambiguous query")
	}
	if got := evalString(t, m, ctx, `document.title`); got != "find+act" {
		t.Fatalf("title = %q, want it untouched", got)
	}
}

// find_act inside a batch, on the direct-CDP transport. The step reports the ref
// it chose so the caller can tell what the batch acted on.
func TestBatchFindActStepResolvesAndActs(t *testing.T) {
	m, ctx, cancel := newFindActTab(t)
	defer cancel()

	result, err := m.ExecuteBatch(ctx, []BatchStep{
		{Action: "find_act", Find: &FindAct{Query: "Email", Role: "textbox", Action: "fill", Value: "fixture-user"}},
		{Action: "find_act", Find: &FindAct{Query: "Check out", Role: "button", Action: "click"}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !result.OK {
		t.Fatalf("batch not ok: %+v", result)
	}
	for _, step := range result.Steps {
		if step.Ref == "" {
			t.Fatalf("find_act step %d reported no ref: %+v", step.Index, step)
		}
	}
	if got := evalString(t, m, ctx, `document.getElementById('email').value`); got != "fixture-user" {
		t.Fatalf("email value = %q, want %q", got, "fixture-user")
	}
	if got := evalString(t, m, ctx, `document.title`); got != "only clicked" {
		t.Fatalf("title = %q, want %q", got, "only clicked")
	}
}

// An ambiguous find_act step fails the batch at that step rather than acting on
// the best match and letting the flow carry on believing it worked.
func TestBatchFindActStepFailsOnAmbiguity(t *testing.T) {
	m, ctx, cancel := newFindActTab(t)
	defer cancel()

	result, err := m.ExecuteBatch(ctx, []BatchStep{
		{Action: "find_act", Find: &FindAct{Query: "Add", Role: "button", Action: "click"}},
		{Action: "find_act", Find: &FindAct{Query: "Check out", Role: "button", Action: "click"}},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if result.OK {
		t.Fatalf("ambiguous find_act step passed: %+v", result)
	}
	if !strings.Contains(result.Error, "refusing to guess") {
		t.Fatalf("batch error = %q, want the ambiguity refusal", result.Error)
	}
	if result.StepsCompleted != 1 {
		t.Fatalf("steps_completed = %d, want 1 (the failing step, with the rest not run)", result.StepsCompleted)
	}
	if got := evalString(t, m, ctx, `document.title`); got != "find+act" {
		t.Fatalf("title = %q, want it untouched — the batch clicked something after refusing to", got)
	}
}

// A find_act step inside a plan carries the same rule and reports the element it
// chose in its step result.
func TestPlanFindActStepResolvesAndActs(t *testing.T) {
	m, ctx, cancel := newFindActTab(t)
	defer cancel()

	result, err := m.ExecutePlan(ctx, []PlanStep{
		{Action: "find_act", Find: &FindAct{Query: "Check out", Role: "button", Action: "click"}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !result.OK {
		t.Fatalf("plan not ok: %+v", result)
	}
	step := result.Steps[0]
	findResult, ok := step.Result.(FindActResult)
	if !ok {
		t.Fatalf("plan step result is %T, want FindActResult", step.Result)
	}
	if findResult.Matched.Ref == "" || findResult.Action != "click" {
		t.Fatalf("plan find_act result = %+v", findResult)
	}
	if got := evalString(t, m, ctx, `document.title`); got != "only clicked" {
		t.Fatalf("title = %q, want %q", got, "only clicked")
	}
}
