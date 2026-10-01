package browser

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode"
)

// PacingMode says whether agent actions are spaced and typed like a person.
type PacingMode string

const (
	PacingOff   PacingMode = "off"
	PacingHuman PacingMode = "human"
)

// ParsePacing accepts "off" or "human"; empty means off.
func ParsePacing(value string) (PacingMode, error) {
	switch PacingMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", PacingOff:
		return PacingOff, nil
	case PacingHuman:
		return PacingHuman, nil
	}
	return PacingOff, fmt.Errorf("pacing must be %q or %q, got %q", PacingOff, PacingHuman, value)
}

const (
	actionGapMedian = 700 * time.Millisecond
	actionGapMin    = 300 * time.Millisecond
	actionGapMax    = 2500 * time.Millisecond
	keyDelayMedian  = 85 * time.Millisecond
	keyDelayMin     = 35 * time.Millisecond
	keyDelayMax     = 260 * time.Millisecond
	perRuneLimit    = 80
	typingBudget    = 6 * time.Second
	maxTrackedTabs  = 256
)

// Pacer spaces agent actions on each tab and types text at a human cadence.
// A nil Pacer, or one in PacingOff, changes nothing.
type Pacer struct {
	mode  PacingMode
	mu    sync.Mutex
	next  map[string]time.Time
	rng   *rand.Rand
	sleep func(context.Context, time.Duration) error
	now   func() time.Time
}

func NewPacer(mode PacingMode) *Pacer {
	return &Pacer{
		mode:  mode,
		next:  map[string]time.Time{},
		rng:   rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), rand.Uint64())),
		sleep: sleepContext,
		now:   time.Now,
	}
}

func (p *Pacer) Mode() PacingMode {
	if p == nil {
		return PacingOff
	}
	return p.mode
}

func (p *Pacer) human() bool { return p != nil && p.mode == PacingHuman }

// BeforeAction waits until a random human gap has passed since the previous
// action on tab. Concurrent actions on one tab queue behind each other; an
// agent already slower than the gap waits nothing.
func (p *Pacer) BeforeAction(ctx context.Context, tab string) error {
	if !p.human() {
		return nil
	}
	p.mu.Lock()
	now := p.now()
	start := p.next[tab]
	if start.Before(now) {
		start = now
	}
	p.next[tab] = start.Add(p.lognormal(actionGapMedian, 0.45, actionGapMin, actionGapMax))
	p.pruneLocked(now)
	p.mu.Unlock()
	return p.sleep(ctx, start.Sub(now))
}

// BeforeSequenceStep paces UI actions while passive observations preserve the action clock.
func (p *Pacer) BeforeSequenceStep(ctx context.Context, tab, action string) error {
	switch action {
	case "wait", "read", "snapshot", "assert", "assert_visible", "assert_text", "assert_value", "assert_hidden":
		return ctx.Err()
	default:
		return p.BeforeAction(ctx, tab)
	}
}

// Type sends text through insert: one character at a time with key delays for
// short text, and word-sized chunks for text longer than perRuneLimit.
func (p *Pacer) Type(ctx context.Context, text string, insert func(string) error) error {
	if !p.human() || text == "" {
		return insert(text)
	}
	chunks := typingChunks(text)
	delays := make([]time.Duration, len(chunks))
	var total time.Duration
	for i := 1; i < len(chunks); i++ {
		delays[i] = p.chunkDelay(chunks[i-1])
		total += delays[i]
	}
	scale := 1.0
	if total > typingBudget {
		scale = float64(typingBudget) / float64(total)
	}
	for i, chunk := range chunks {
		if err := p.sleep(ctx, time.Duration(float64(delays[i])*scale)); err != nil {
			return err
		}
		if err := insert(chunk); err != nil {
			return err
		}
	}
	return nil
}

func typingChunks(text string) []string {
	runes := []rune(text)
	if len(runes) <= perRuneLimit {
		out := make([]string, len(runes))
		for i, r := range runes {
			out[i] = string(r)
		}
		return out
	}
	var out []string
	start := 0
	for i, r := range runes {
		if unicode.IsSpace(r) {
			out = append(out, string(runes[start:i+1]))
			start = i + 1
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

func (p *Pacer) chunkDelay(previous string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := len([]rune(previous))
	if keys > 1 {
		keys = 1 + keys/3
	}
	var delay time.Duration
	for range keys {
		delay += p.lognormal(keyDelayMedian, 0.35, keyDelayMin, keyDelayMax)
	}
	last, _ := lastRune(previous)
	if unicode.IsSpace(last) || unicode.IsPunct(last) {
		delay += time.Duration(p.rng.Int64N(int64(250 * time.Millisecond)))
	}
	if p.rng.Float64() < 0.03 {
		delay += 300*time.Millisecond + time.Duration(p.rng.Int64N(int64(600*time.Millisecond)))
	}
	return delay
}

func lastRune(s string) (rune, bool) {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0, false
	}
	return runes[len(runes)-1], true
}

func (p *Pacer) lognormal(median time.Duration, sigma float64, lo, hi time.Duration) time.Duration {
	d := time.Duration(float64(median) * math.Exp(sigma*p.rng.NormFloat64()))
	return min(max(d, lo), hi)
}

func (p *Pacer) pruneLocked(now time.Time) {
	if len(p.next) <= maxTrackedTabs {
		return
	}
	for tab, at := range p.next {
		if now.Sub(at) > time.Minute {
			delete(p.next, tab)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SetPacing makes this manager space and type agent actions like a person.
func (m *Manager) SetPacing(mode PacingMode) { m.pacer = NewPacer(mode) }

// Pacing reports the pacing mode in force.
func (m *Manager) Pacing() PacingMode { return m.pacer.Mode() }
