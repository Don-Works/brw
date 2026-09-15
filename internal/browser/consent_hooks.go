package browser

import (
	"context"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// The runtime half of site consent.
//
// Most of the gate decides from a call's arguments before anything is
// dispatched, which is where a refusal costs nothing. Two things cannot be
// decided there, and both are carried on the context so a controller that knows
// nothing about consent keeps working unchanged:
//
//   - A plan or batch step runs after earlier steps have already moved the page.
//     Where it lands is a fact only the runner holds, and only at the moment the
//     step runs.
//   - A URL the daemon fetches itself can answer with a redirect. The hop is a
//     destination no argument named, and only the HTTP client sees it.
//
// A surface installs these when it dispatches; a context without them gates
// nothing, exactly as before consent existed.

type sequenceGateKey struct{}

type fetchCheckKey struct{}

type frameReadCheckKey struct{}

// SequenceGate re-checks one plan or batch step immediately before it runs.
// index is the step's position in the call, which is what decides whether its
// high-risk confirmation was already asked at dispatch.
type SequenceGate func(index int, tabID string, step siteconsent.StepProbe) error

// FetchCheck gates a URL the DAEMON retrieves itself rather than the page,
// including every redirect hop. A grant is for an origin, not for a request, so
// a 302 from a granted site to an un-granted one is a read of a site nobody
// consented to.
type FetchCheck func(rawURL string) error

// FrameReadCheck gates reading the DOCUMENT inside a cross-origin iframe.
//
// include_frames attaches a CDP session to the frame's own target and runs the
// walker in a THIRD PARTY's document — the payment form, the embedded editor,
// the social widget. The call was gated against the embedder's origin, which is
// not a grant to read what the embedder happens to have embedded, so each frame
// origin is checked on its own. A frame this refuses is still reported, as the
// clickable box it was before include_frames could read it at all.
type FrameReadCheck func(frameOrigin string) error

// WithSequenceGate installs the per-step consent re-check for one sequence call.
func WithSequenceGate(ctx context.Context, gate SequenceGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, sequenceGateKey{}, gate)
}

// GateSequenceStep applies the installed per-step re-check, if there is one. It
// is exported because the extension bridge runs the same step verbs through its
// own runners, and a gate one transport honours and the other does not is a
// bypass by choice of transport.
func GateSequenceStep(ctx context.Context, index int, tabID string, step siteconsent.StepProbe) error {
	gate, ok := ctx.Value(sequenceGateKey{}).(SequenceGate)
	if !ok || gate == nil {
		return nil
	}
	return gate(index, tabID, step)
}

// WithFetchCheck installs the daemon-side fetch gate.
func WithFetchCheck(ctx context.Context, check FetchCheck) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, fetchCheckKey{}, check)
}

// FetchCheckFromContext returns the installed daemon-side fetch gate, or nil.
func FetchCheckFromContext(ctx context.Context) FetchCheck {
	check, ok := ctx.Value(fetchCheckKey{}).(FetchCheck)
	if !ok {
		return nil
	}
	return check
}

// WithFrameReadCheck installs the cross-origin frame read gate.
func WithFrameReadCheck(ctx context.Context, check FrameReadCheck) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, frameReadCheckKey{}, check)
}

// FrameReadCheckFromContext returns the installed frame read gate, or nil. A
// context without one gates nothing, exactly as before consent existed.
func FrameReadCheckFromContext(ctx context.Context) FrameReadCheck {
	check, ok := ctx.Value(frameReadCheckKey{}).(FrameReadCheck)
	if !ok {
		return nil
	}
	return check
}

// carryConsentHooks copies the consent hooks from one context onto another.
//
// A per-tab CDP context is long-lived and derived from the browser allocator,
// not from the call that uses it, so a hook the caller installed does not reach
// it on its own. Every place that swaps a call's context for a tab's has to
// carry them across or the gate silently stops running there.
func carryConsentHooks(from, to context.Context) context.Context {
	if gate, ok := from.Value(sequenceGateKey{}).(SequenceGate); ok && gate != nil {
		to = context.WithValue(to, sequenceGateKey{}, gate)
	}
	if check, ok := from.Value(fetchCheckKey{}).(FetchCheck); ok && check != nil {
		to = context.WithValue(to, fetchCheckKey{}, check)
	}
	if check, ok := from.Value(frameReadCheckKey{}).(FrameReadCheck); ok && check != nil {
		to = context.WithValue(to, frameReadCheckKey{}, check)
	}
	return to
}

// ConsentProbe describes a plan step to the consent gate.
func (s PlanStep) ConsentProbe() siteconsent.StepProbe {
	return siteconsent.StepProbe{
		Action:    s.Action,
		URL:       s.URL,
		ID:        s.ID,
		Ref:       s.Ref,
		Text:      s.Text,
		Value:     s.Value,
		Condition: s.Condition,
	}
}

// ConsentProbe describes a batch step to the consent gate.
func (s BatchStep) ConsentProbe() siteconsent.StepProbe {
	return siteconsent.StepProbe{
		Action:    s.Action,
		URL:       s.URL,
		ID:        s.ID,
		Ref:       s.Ref,
		Text:      s.Text,
		Value:     s.Value,
		Condition: s.Condition,
	}
}
