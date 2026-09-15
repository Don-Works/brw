package browser

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// carriedHookProbes maps each method of ConsentEnforcer to the way a context is
// asked whether that hook reached it.
//
// It is keyed by method name so the interface is the list: a hook added to
// ConsentEnforcer fails the coverage check below until someone says how to
// observe it, and then fails the carry check until carryConsentHooks copies it.
var carriedHookProbes = map[string]func(context.Context) bool{
	"CheckFetchDestination": func(ctx context.Context) bool { return FetchCheckFromContext(ctx) != nil },
	"CheckFrameRead":        func(ctx context.Context) bool { return FrameReadCheckFromContext(ctx) != nil },
}

type stubEnforcer struct{}

func (stubEnforcer) CheckFetchDestination(string) error { return errors.New("refused") }

func (stubEnforcer) CheckFrameRead(string) error { return errors.New("refused") }

// TestCarryConsentHooksCarriesEveryRuntimeHook is the other half of the surface
// parity test, one layer down.
//
// A surface installs the hooks on the CALL's context. A per-tab CDP context is
// long-lived and derived from the browser allocator instead, so every place that
// swaps one for the other has to copy the hooks across or the gate stops running
// there — a bypass by choice of code path rather than by choice of surface, and
// invisible either way. carryConsentHooks is a hand-written list of three keys,
// so the enumeration is against ConsentEnforcer rather than against itself.
func TestCarryConsentHooksCarriesEveryRuntimeHook(t *testing.T) {
	iface := reflect.TypeOf((*ConsentEnforcer)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		if _, ok := carriedHookProbes[name]; !ok {
			t.Errorf("ConsentEnforcer.%s is a runtime consent hook with no probe: say how a context is asked whether it arrived, so carryConsentHooks can be held to it", name)
		}
	}
	for name := range carriedHookProbes {
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("a probe names %s, which ConsentEnforcer no longer declares", name)
		}
	}

	gated := 0
	from := WithRuntimeConsent(context.Background(), stubEnforcer{})
	from = WithSequenceGate(from, func(int, string, siteconsent.StepProbe) error {
		gated++
		return errors.New("refused")
	})
	for name, installed := range carriedHookProbes {
		if !installed(from) {
			t.Fatalf("WithRuntimeConsent did not install %s, so this test would prove nothing about carrying it", name)
		}
	}

	to := carryConsentHooks(from, context.Background())
	for name, installed := range carriedHookProbes {
		if !installed(to) {
			t.Errorf("carryConsentHooks dropped %s, so a call that swaps the caller's context for a tab's stops asking that question entirely", name)
		}
	}
	if err := GateSequenceStep(to, 0, "tab-1", siteconsent.StepProbe{Action: "click"}); err == nil {
		t.Error("carryConsentHooks dropped the per-step sequence gate, so a plan's steps run un-rechecked on a tab context")
	}
	if gated != 1 {
		t.Errorf("the carried sequence gate was called %d times, want once", gated)
	}
}
