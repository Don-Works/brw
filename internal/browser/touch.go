package browser

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// TouchController is the optional transport capability for synthesized touch
// gestures. It is separate from Controller so a lane that cannot dispatch touch
// fails with a named error rather than not compiling.
type TouchController interface {
	Touch(context.Context, TouchOptions) (ActionResult, error)
}

// ErrTouchUnsupported is returned by transports that cannot synthesize touch
// input.
var ErrTouchUnsupported = errors.New("touch gestures are not supported on this transport")

// TouchOptions is a tap or a swipe. A tap presses and releases at one point; a
// swipe presses at one point, moves through interpolated steps, and releases at
// another. Pair with brw_emulate_device (touch:true) when the page decides its
// layout from touch capability, but note the gesture itself does not require it.
type TouchOptions struct {
	// Action is tap or swipe.
	Action string `json:"action"`
	// Ref and X/Y name the start point. Ref wins when both are supplied.
	Ref string   `json:"ref,omitempty"`
	X   *float64 `json:"x,omitempty"`
	Y   *float64 `json:"y,omitempty"`
	// ToRef and ToX/ToY name the end point of a swipe.
	ToRef string   `json:"to_ref,omitempty"`
	ToX   *float64 `json:"to_x,omitempty"`
	ToY   *float64 `json:"to_y,omitempty"`
	// DurationMS is how long a swipe takes, in milliseconds. Defaults to 300 and
	// is capped at 10000.
	DurationMS int `json:"duration_ms,omitempty"`
	// Repeat performs the gesture this many times (1-100) in one call.
	Repeat int    `json:"repeat,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// NormalizeTouch validates a gesture request and resolves its defaults.
func NormalizeTouch(opts TouchOptions) (TouchOptions, error) {
	out := opts
	out.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	if out.Action == "" {
		out.Action = "tap"
	}
	switch out.Action {
	case "tap", "swipe":
	default:
		return out, fmt.Errorf("unknown touch action %q: use tap or swipe", opts.Action)
	}
	if !hasTouchPoint(opts.Ref, opts.X, opts.Y) {
		return out, errors.New("touch needs a start point: pass ref, or both x and y")
	}
	if out.Action == "swipe" {
		if !hasTouchPoint(opts.ToRef, opts.ToX, opts.ToY) {
			return out, errors.New("a swipe needs an end point: pass to_ref, or both to_x and to_y")
		}
		if out.DurationMS == 0 {
			out.DurationMS = defaultSwipeDurationMS
		}
	}
	if out.DurationMS < 0 {
		return out, fmt.Errorf("duration_ms %d is negative", out.DurationMS)
	}
	if out.DurationMS > maxSwipeDurationMS {
		return out, fmt.Errorf("duration_ms %d is over the %d ms limit", out.DurationMS, maxSwipeDurationMS)
	}
	if out.Repeat == 0 {
		out.Repeat = 1
	}
	if out.Repeat < 1 || out.Repeat > maxTouchRepeat {
		return out, fmt.Errorf("repeat %d is outside the range 1-%d", out.Repeat, maxTouchRepeat)
	}
	for name, value := range map[string]*float64{"x": opts.X, "y": opts.Y, "to_x": opts.ToX, "to_y": opts.ToY} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return out, fmt.Errorf("%s is not a finite number", name)
		}
	}
	return out, nil
}

func hasTouchPoint(ref string, x, y *float64) bool {
	if strings.TrimSpace(ref) != "" {
		return true
	}
	return x != nil && y != nil
}

const (
	defaultSwipeDurationMS = 300
	maxSwipeDurationMS     = 10000
	maxTouchRepeat         = 100
	touchMoveSteps         = 12
)
