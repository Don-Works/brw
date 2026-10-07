package siteconsent

import "strings"

// Posture is what a daemon tells an unattended caller about whether a request through it could stop and ask a human before it completes.
type Posture struct {
	// Enabled reports that origins are gated at all.
	Enabled bool `json:"enabled"`
	// Interactive reports that somewhere in the chain a daemon has a prompter on its terminal and will stop and ask rather than refuse.
	Interactive bool `json:"interactive"`
	// ConfirmActions reports that a high-risk action needs confirmation.
	ConfirmActions bool `json:"confirm_actions"`
	// Unknown reports that some daemon in the chain could not be asked, so Interactive is a floor rather than the answer.
	Unknown bool `json:"unknown,omitempty"`
	// Reason names what could not be asked, for the operator who has to fix it.
	Reason string `json:"unknown_reason,omitempty"`
}

// Posture reports this guard's own contribution to the chain's posture.
func (g *Guard) Posture() Posture {
	return Posture{
		Enabled:        g.Enabled(),
		Interactive:    g.Interactive(),
		ConfirmActions: g.ConfirmActions(),
	}
}

// Merge folds the posture of a daemon this one forwards to into its own.
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
