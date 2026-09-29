package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fakePacer(mode PacingMode) (*Pacer, *time.Time, *[]time.Duration) {
	p := NewPacer(mode)
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	var slept []time.Duration
	p.now = func() time.Time { return now }
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(max(d, 0))
		return nil
	}
	return p, &now, &slept
}

func TestPacingOffNeverWaits(t *testing.T) {
	for _, p := range []*Pacer{nil, NewPacer(PacingOff)} {
		if err := p.BeforeAction(context.Background(), "tab"); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := p.Type(context.Background(), "hello", func(s string) error { got = append(got, s); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "hello" {
			t.Fatalf("pacing off must insert text in one call, got %q", got)
		}
	}
}

func TestHumanPacingSpacesBackToBackActions(t *testing.T) {
	p, _, slept := fakePacer(PacingHuman)
	for range 50 {
		if err := p.BeforeAction(context.Background(), "tab"); err != nil {
			t.Fatal(err)
		}
	}
	if (*slept)[0] != 0 {
		t.Fatalf("the first action must not wait, waited %s", (*slept)[0])
	}
	distinct := map[time.Duration]bool{}
	for _, d := range (*slept)[1:] {
		if d < actionGapMin || d > actionGapMax {
			t.Fatalf("gap %s outside [%s, %s]", d, actionGapMin, actionGapMax)
		}
		distinct[d] = true
	}
	if len(distinct) < 10 {
		t.Fatalf("gaps are not random: %d distinct values in 49", len(distinct))
	}
}

func TestHumanPacingDoesNotSlowAnAlreadySlowAgent(t *testing.T) {
	p, now, slept := fakePacer(PacingHuman)
	_ = p.BeforeAction(context.Background(), "tab")
	*now = now.Add(5 * time.Second)
	_ = p.BeforeAction(context.Background(), "tab")
	if (*slept)[1] != 0 {
		t.Fatalf("an action 5s after the last waited %s", (*slept)[1])
	}
}

func TestHumanPacingKeepsTabsIndependent(t *testing.T) {
	p, _, slept := fakePacer(PacingHuman)
	_ = p.BeforeAction(context.Background(), "a")
	_ = p.BeforeAction(context.Background(), "b")
	if (*slept)[1] != 0 {
		t.Fatalf("an action on another tab waited %s", (*slept)[1])
	}
}

func TestHumanTypingSendsCharactersWithKeyDelays(t *testing.T) {
	p, _, slept := fakePacer(PacingHuman)
	var got []string
	if err := p.Type(context.Background(), "hi there", func(s string) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "hi there" || len(got) != len("hi there") {
		t.Fatalf("chunks = %q", got)
	}
	for _, d := range (*slept)[1:] {
		if d < keyDelayMin {
			t.Fatalf("key delay %s below %s", d, keyDelayMin)
		}
	}
}

func TestHumanTypingOfLongTextStaysWithinBudget(t *testing.T) {
	p, _, slept := fakePacer(PacingHuman)
	text := strings.Repeat("a rather long sentence, typed by an agent. ", 40)
	var got []string
	if err := p.Type(context.Background(), text, func(s string) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != text {
		t.Fatal("long text was not reassembled exactly")
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if total > typingBudget+time.Millisecond {
		t.Fatalf("typing took %s, budget %s", total, typingBudget)
	}
}

func TestHumanPacingStopsWhenTheCallIsCancelled(t *testing.T) {
	p := NewPacer(PacingHuman)
	_ = p.BeforeAction(context.Background(), "tab")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.BeforeAction(ctx, "tab"); err == nil {
		t.Fatal("a cancelled call must stop waiting")
	}
}

func TestParsePacing(t *testing.T) {
	for in, want := range map[string]PacingMode{"": PacingOff, "off": PacingOff, "HUMAN": PacingHuman} {
		got, err := ParsePacing(in)
		if err != nil || got != want {
			t.Fatalf("ParsePacing(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParsePacing("stealth"); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}
