package browser

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Don-Works/brw/internal/siteconsent"
)

var carriedHookProbes = map[string]func(context.Context) bool{
	"CheckFetchDestination": func(ctx context.Context) bool { return FetchCheckFromContext(ctx) != nil },
	"CheckFrameRead":        func(ctx context.Context) bool { return FrameReadCheckFromContext(ctx) != nil },
	"CheckFrameAct":         func(ctx context.Context) bool { return FrameActCheckFromContext(ctx) != nil },
}

type stubEnforcer struct{}

func (stubEnforcer) CheckFetchDestination(string) error { return errors.New("refused") }

func (stubEnforcer) CheckFrameRead(string) error { return errors.New("refused") }

func (stubEnforcer) CheckFrameAct(string) error { return errors.New("refused") }

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

func TestAFrameReadHookCannotAuthorizeAnAction(t *testing.T) {
	reads := 0
	ctx := WithFrameReadCheck(context.Background(), func(string) error { reads++; return nil })
	if _, err := (&Manager{}).clickCrossOriginFrameRef(ctx, "f0:e1"); !errors.Is(err, ErrFrameActCheckMissing) {
		t.Fatalf("frame action with only a read hook = %v", err)
	}
	if reads != 0 {
		t.Fatalf("frame action asked a read gate %d times", reads)
	}
}
