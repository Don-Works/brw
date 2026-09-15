package siteconsent

import "strings"

// Posture is what a daemon tells an unattended caller about whether a request
// through it could stop and ask a human before it completes.
//
// It is the whole CHAIN's answer, not one process's. brw daemons compose: an
// --upstream-http proxy serves /health from its own guard and forwards the work
// to a daemon with a guard of its own, and a scheduled run reaching the proxy
// blocks on whichever of them has a prompter. A posture assembled from one
// process's flags therefore answers a question nobody asked — which is how
// `brw run`'s fail-closed refusal came to be bypassed by putting a proxy in
// front of the prompting daemon.
type Posture struct {
	// Enabled reports that origins are gated at all.
	Enabled bool `json:"enabled"`
	// Interactive reports that somewhere in the chain a daemon has a prompter
	// on its terminal and will stop and ask rather than refuse.
	Interactive bool `json:"interactive"`
	// ConfirmActions reports that a high-risk action needs confirmation.
	ConfirmActions bool `json:"confirm_actions"`
	// Unknown reports that some daemon in the chain could not be asked, so
	// Interactive is a floor rather than the answer. A caller that must not
	// block treats it exactly as it treats Interactive: "unknown" and "yes" are
	// the same answer to "could this hang".
	Unknown bool `json:"unknown,omitempty"`
	// Reason names what could not be asked, for the operator who has to fix it.
	Reason string `json:"unknown_reason,omitempty"`
}

// Posture reports this guard's own contribution to the chain's posture. A nil
// guard is a daemon with no consent gate, which is a definite answer rather
// than an unknown one.
func (g *Guard) Posture() Posture {
	return Posture{
		Enabled:        g.Enabled(),
		Interactive:    g.Interactive(),
		ConfirmActions: g.ConfirmActions(),
	}
}

// Merge folds the posture of a daemon this one forwards to into its own.
//
// Every field is an OR because either end can be the one that stops and asks:
// the proxy applies its own guard to the request before forwarding it, and the
// daemon behind applies its own to the work. The safe summary of two daemons is
// the stricter of the two on every axis, and an unknown hop poisons the whole
// chain's answer rather than being dropped.
func (p Posture) Merge(other Posture) Posture {
	merged := Posture{
		Enabled:        p.Enabled || other.Enabled,
		Interactive:    p.Interactive || other.Interactive,
		ConfirmActions: p.ConfirmActions || other.ConfirmActions,
		Unknown:        p.Unknown || other.Unknown,
	}
	reasons := make([]string, 0, 2)
	for _, reason := range []string{p.Reason, other.Reason} {
		if strings.TrimSpace(reason) != "" {
			reasons = append(reasons, strings.TrimSpace(reason))
		}
	}
	merged.Reason = strings.Join(reasons, "; ")
	return merged
}

// UnreadablePosture is the posture of a hop that could not be asked at all.
func UnreadablePosture(reason string) Posture {
	return Posture{Unknown: true, Reason: strings.TrimSpace(reason)}
}
