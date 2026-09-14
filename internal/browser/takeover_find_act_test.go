package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A find_act step is guarded under the verb it will run, so it has no refusal
// case of its own in the guarded-action inventory. That delegation is only safe
// if it actually refuses: a batch already running when the human took over must
// not keep locating and clicking for the rest of its length.
func TestFindActStepIsRefusedDuringTakeover(t *testing.T) {
	tests := []struct {
		name       string
		step       BatchStep
		wantAction string
	}{
		{
			name:       "click",
			step:       BatchStep{Action: "find_act", Find: &FindAct{Query: "Add", Role: "button", Action: "click"}},
			wantAction: "click",
		},
		{
			name:       "fill",
			step:       BatchStep{Action: "find_act", Find: &FindAct{Query: "Email", Action: "fill", Value: "fixture-user"}},
			wantAction: "fill",
		},
		{
			// Fail closed: a step with no find payload must still be refused,
			// under its own name, rather than fall through the delegation.
			name:       "no find payload",
			step:       BatchStep{Action: "find_act"},
			wantAction: "find_act",
		},
		{
			// And an inner verb the actuator does not know must not become an
			// exemption either.
			name:       "unknown inner verb",
			step:       BatchStep{Action: "find_act", Find: &FindAct{Query: "Add", Action: "submit"}},
			wantAction: "submit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserlessManager()
			if _, err := m.AcquireTakeover("operator", time.Minute); err != nil {
				t.Fatalf("acquire takeover: %v", err)
			}
			result := m.executeBatchStep(context.Background(), "tab-1", 0, tt.step)
			if result.OK {
				t.Fatal("a find_act step ran while the human held the browser")
			}
			if !strings.Contains(result.Error, "operator") {
				t.Fatalf("step error %q does not name the holder", result.Error)
			}
			if !strings.Contains(result.Error, tt.wantAction) {
				t.Fatalf("step error %q does not name the refused verb %q", result.Error, tt.wantAction)
			}
		})
	}
}

// Releasing the hold lets the step through the guard again — it then fails on
// the browser this Manager does not have, which is a different failure.
func TestFindActStepIsAllowedAfterRelease(t *testing.T) {
	m := newBrowserlessManager()
	grant, err := m.AcquireTakeover("operator", time.Minute)
	if err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}
	if err := m.ReleaseTakeover(grant.Token); err != nil {
		t.Fatalf("release takeover: %v", err)
	}
	step := BatchStep{Action: "find_act", Find: &FindAct{Query: "Add", Role: "button", Action: "click"}}
	result := m.executeBatchStep(context.Background(), "tab-1", 0, step)
	if strings.Contains(result.Error, "operator") {
		t.Fatalf("the step is still refused after release: %q", result.Error)
	}
}
