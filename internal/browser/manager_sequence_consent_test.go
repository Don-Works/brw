package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

// sequenceFixture opens a page with one field and one button and returns their
// refs, so a sequence has something real to fill and click.
func sequenceFixture(t *testing.T) (*Manager, context.Context, context.CancelFunc, string, string, string) {
	t.Helper()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	setup := `document.body.innerHTML='<input aria-label="Name"><button>Apply</button><div id="done"></div>';` +
		`document.querySelector('button').onclick=function(){document.querySelector('#done').textContent='clicked'}; true`
	if _, err := m.Evaluate(tabCtx, setup); err != nil {
		cancel()
		t.Fatal(err)
	}
	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var inputRef, buttonRef string
	for _, element := range snap.Elements {
		switch element.Role {
		case "textbox":
			inputRef = element.Ref
		case "button":
			buttonRef = element.Ref
		}
	}
	if inputRef == "" || buttonRef == "" {
		cancel()
		t.Fatalf("missing refs: %+v", snap.Elements)
	}
	return m, tabCtx, cancel, opened.Tab.ID, inputRef, buttonRef
}

// TestBatchStepsPassThroughTheConsentGate drives the real batch runner and
// proves the per-step gate reaches it.
//
// The gate exists because a batch is authorised once, from arguments that stop
// being true as soon as a step navigates. That is only true if the runner
// actually asks, before the step runs: a refusal that arrives after the click
// has already happened is not a gate.
func TestBatchStepsPassThroughTheConsentGate(t *testing.T) {
	m, tabCtx, cancel, tabID, inputRef, buttonRef := sequenceFixture(t)
	defer cancel()

	var asked []string
	var sawTabIDs []string
	gated := WithSequenceGate(tabCtx, func(index int, stepTabID string, step siteconsent.StepProbe) error {
		asked = append(asked, fmt.Sprintf("%d:%s", index, step.Action))
		sawTabIDs = append(sawTabIDs, stepTabID)
		if step.Action == "click" {
			return errors.New("fixture gate: no grant for the origin this step lands on")
		}
		return nil
	})

	result, err := m.ExecuteBatch(gated, []BatchStep{
		{Action: "fill", Ref: inputRef, Text: "Ada"},
		{Action: "click", Ref: buttonRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatalf("the batch reported success with a step the gate refused: %+v", result)
	}
	if !strings.Contains(result.Error, "fixture gate") {
		t.Fatalf("the batch failed with %q, not the gate's refusal", result.Error)
	}
	if want := []string{"0:fill", "1:click"}; strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Fatalf("the gate was asked about %v, want %v", asked, want)
	}
	for _, seen := range sawTabIDs {
		if seen != tabID {
			t.Fatalf("the gate was given tab %q, not the tab the batch runs on (%q)", seen, tabID)
		}
	}
	// The refused click must not have run: the gate has to precede the action,
	// not report on it.
	clicked, err := m.Evaluate(tabCtx, `document.querySelector('#done').textContent`)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := clicked.(string); strings.TrimSpace(text) != "" {
		t.Fatalf("the refused click still ran: #done = %q", text)
	}
	// The allowed step ran, so this is a gate and not a batch that stopped.
	typed, err := m.Evaluate(tabCtx, `document.querySelector('input').value`)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := typed.(string); strings.TrimSpace(text) != "Ada" {
		t.Fatalf("the allowed fill did not run: input = %q", text)
	}
}

// TestPlanStepsPassThroughTheConsentGate is the same proof for the plan runner,
// which is a second dispatch surface for the same verbs.
func TestPlanStepsPassThroughTheConsentGate(t *testing.T) {
	m, tabCtx, cancel, tabID, inputRef, buttonRef := sequenceFixture(t)
	defer cancel()

	var asked []string
	gated := WithSequenceGate(tabCtx, func(index int, stepTabID string, step siteconsent.StepProbe) error {
		asked = append(asked, fmt.Sprintf("%d:%s", index, step.Action))
		if stepTabID != tabID {
			t.Errorf("the gate was given tab %q, not the tab the plan runs on (%q)", stepTabID, tabID)
		}
		if step.Action == "click" {
			return errors.New("fixture gate: no grant for the origin this step lands on")
		}
		return nil
	})

	result, err := m.ExecutePlan(gated, []PlanStep{
		{Action: "fill", Ref: inputRef, Text: "Ada"},
		{Action: "click", Ref: buttonRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatalf("the plan reported success with a step the gate refused: %+v", result)
	}
	if !strings.Contains(result.Error, "fixture gate") {
		t.Fatalf("the plan failed with %q, not the gate's refusal", result.Error)
	}
	if want := []string{"0:fill", "1:click"}; strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Fatalf("the gate was asked about %v, want %v", asked, want)
	}
	clicked, err := m.Evaluate(tabCtx, `document.querySelector('#done').textContent`)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := clicked.(string); strings.TrimSpace(text) != "" {
		t.Fatalf("the refused click still ran: #done = %q", text)
	}
}

// TestASequenceWithNoGateIsUnchanged is the opt-in guarantee: a daemon with no
// consent store installs no gate, and the runners behave exactly as they did.
func TestASequenceWithNoGateIsUnchanged(t *testing.T) {
	m, tabCtx, cancel, _, inputRef, buttonRef := sequenceFixture(t)
	defer cancel()

	result, err := m.ExecuteBatch(tabCtx, []BatchStep{
		{Action: "fill", Ref: inputRef, Text: "Ada"},
		{Action: "click", Ref: buttonRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.StepsCompleted != 2 {
		t.Fatalf("an ungated batch did not complete: %+v", result)
	}
	clicked, err := m.Evaluate(tabCtx, `document.querySelector('#done').textContent`)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := clicked.(string); strings.TrimSpace(text) != "clicked" {
		t.Fatalf("the ungated click did not run: #done = %q", text)
	}
}
