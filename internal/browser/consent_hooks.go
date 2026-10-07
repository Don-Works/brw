package browser

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/Don-Works/brw/internal/siteconsent"
)

type sequenceGateKey struct{}

type fetchCheckKey struct{}

type frameReadCheckKey struct{}

type frameActCheckKey struct{}

// SequenceGate rechecks a step immediately before execution, using its original index for confirmation.
type SequenceGate func(index int, tabID string, step siteconsent.StepProbe) error

// FetchCheck gates a URL the DAEMON retrieves itself rather than the page, including every redirect hop.
type FetchCheck func(rawURL string) error

// FrameReadCheck gates page and cross-origin document reads against each committed origin.
type FrameReadCheck func(frameOrigin string) error

// CheckDownloadSource checks a download's verifiable HTTP source against the runtime read hook.
func CheckDownloadSource(ctx context.Context, source string) error {
	check := FrameReadCheckFromContext(ctx)
	if check == nil {
		return nil
	}
	source = strings.TrimSpace(source)
	parsed, err := url.Parse(source)
	if err == nil && strings.EqualFold(parsed.Scheme, "blob") {
		source = parsed.Opaque
		parsed, err = url.Parse(source)
	}
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" ||
		!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("download source has no verifiable HTTP origin")
	}
	return check(source)
}

// FrameActCheck gates acting inside a cross-origin document against its own origin.
type FrameActCheck func(frameOrigin string) error

// ErrFrameActCheckMissing refuses frame actuation when a caller installed only a read gate.
var ErrFrameActCheckMissing = errors.New("cross-origin frame actions require a runtime act permission check; a frame read grant does not permit acting")

// ConsentEnforcer supplies every runtime destination and frame permission check.
type ConsentEnforcer interface {
	// CheckFetchDestination gates a URL the DAEMON retrieves itself rather than the page, including every redirect hop.
	CheckFetchDestination(rawURL string) error
	// CheckFrameRead gates document reads against the committed origin.
	CheckFrameRead(frameOrigin string) error
	// CheckFrameAct gates input in a cross-origin document independently of read permission.
	CheckFrameAct(frameOrigin string) error
}

// WithRuntimeConsent installs every runtime hook a surface answers.
func WithRuntimeConsent(ctx context.Context, enforcer ConsentEnforcer) context.Context {
	if enforcer == nil {
		return ctx
	}
	ctx = WithFetchCheck(ctx, enforcer.CheckFetchDestination)
	ctx = WithFrameReadCheck(ctx, enforcer.CheckFrameRead)
	ctx = WithFrameActCheck(ctx, enforcer.CheckFrameAct)
	return ctx
}

// WithSequenceGate installs the per-step consent re-check for one sequence call.
func WithSequenceGate(ctx context.Context, gate SequenceGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, sequenceGateKey{}, gate)
}

// GateSequenceStep applies the installed per-step re-check, if there is one.
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
	check, _ := ctx.Value(fetchCheckKey{}).(FetchCheck)
	return check
}

// WithFrameReadCheck installs the cross-origin frame read gate.
func WithFrameReadCheck(ctx context.Context, check FrameReadCheck) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, frameReadCheckKey{}, check)
}

// FrameReadCheckFromContext returns the installed frame read gate, or nil.
func FrameReadCheckFromContext(ctx context.Context) FrameReadCheck {
	check, _ := ctx.Value(frameReadCheckKey{}).(FrameReadCheck)
	return check
}

// WithFrameActCheck installs the cross-origin frame act gate.
func WithFrameActCheck(ctx context.Context, check FrameActCheck) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, frameActCheckKey{}, check)
}

// FrameActCheckFromContext returns the installed cross-origin act gate, or nil.
func FrameActCheckFromContext(ctx context.Context) FrameActCheck {
	check, _ := ctx.Value(frameActCheckKey{}).(FrameActCheck)
	return check
}

func carryConsentHooks(from, to context.Context) context.Context {
	if gate, ok := from.Value(sequenceGateKey{}).(SequenceGate); ok && gate != nil {
		to = WithSequenceGate(to, gate)
	}
	to = WithFetchCheck(to, FetchCheckFromContext(from))
	to = WithFrameReadCheck(to, FrameReadCheckFromContext(from))
	if check := FrameActCheckFromContext(from); check != nil {
		to = WithFrameActCheck(to, check)
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
