package browser

import (
	"context"
	"testing"
	"time"
)

func TestPassiveSequenceStepsPreserveRealActionClock(t *testing.T) {
	p, now, slept := fakePacer(PacingHuman)
	if err := p.BeforeSequenceStep(context.Background(), "tab", "click"); err != nil {
		t.Fatal(err)
	}
	scheduled := p.next["tab"]
	for _, action := range []string{"wait", "read", "snapshot", "assert", "assert_visible", "assert_text", "assert_value", "assert_hidden"} {
		*now = now.Add(time.Millisecond)
		if err := p.BeforeSequenceStep(context.Background(), "tab", action); err != nil {
			t.Fatal(err)
		}
		if p.next["tab"] != scheduled || len(*slept) != 1 {
			t.Fatalf("passive %s changed the real-action clock", action)
		}
	}
	expected := scheduled.Sub(*now)
	if err := p.BeforeSequenceStep(context.Background(), "tab", "click"); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 2 || (*slept)[1] != expected || expected < actionGapMin-8*time.Millisecond {
		t.Fatalf("next actual action gap=%v want=%v", *slept, expected)
	}
}

func TestUnknownAndEvaluationSequenceStepsRemainPaced(t *testing.T) {
	for _, action := range []string{"click", "fill", "type", "select", "press", "scroll", "hover", "open", "navigate_to", "focus_tab", "evaluate", "unknown", "WAIT", "assert_unknown"} {
		p, _, slept := fakePacer(PacingHuman)
		if err := p.BeforeSequenceStep(context.Background(), "tab", action); err != nil {
			t.Fatal(err)
		}
		if err := p.BeforeSequenceStep(context.Background(), "tab", action); err != nil {
			t.Fatal(err)
		}
		if len(*slept) != 2 || (*slept)[1] < actionGapMin {
			t.Fatalf("%s escaped action pacing", action)
		}
	}
}

func TestPassiveSequenceStepsRespectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, p := range []*Pacer{nil, NewPacer(PacingOff), NewPacer(PacingHuman)} {
		if err := p.BeforeSequenceStep(ctx, "tab", "wait"); err != context.Canceled {
			t.Fatalf("cancelled passive step err=%v", err)
		}
	}
}
