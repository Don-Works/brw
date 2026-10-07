package recipe

import (
	"errors"
	"fmt"
)

// SiteIdempotencyFormNonce is the one mechanism brw can actually use: a token the page mints per submission and carries in a form field, which the site checks when the submission arrives.
const SiteIdempotencyFormNonce = "form_nonce"

// SiteIdempotency names the target site's own duplicate-suppression token.
type SiteIdempotency struct {
	Kind string `json:"kind"`
	// Target names the field carrying the token.
	Target *Target `json:"target"`
}

func validateSiteIdempotency(value SiteIdempotency) error {
	if value.Kind != SiteIdempotencyFormNonce {
		return fmt.Errorf("site idempotency kind must be %s, got %q", SiteIdempotencyFormNonce, value.Kind)
	}
	if value.Target == nil {
		return errors.New("site idempotency requires a semantic target naming the nonce field")
	}
	return validateTarget(*value.Target)
}

// RequireWriteVerification refuses a recipe whose external write has no unambiguous read-back.
func RequireWriteVerification(value Recipe) error {
	var problems []error
	for _, step := range value.Steps {
		if step.Effect != "external_write" {
			continue
		}
		if _, err := WriteVerification(value, step); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// WriteVerification returns the assert step that reads back what one write did.
func WriteVerification(value Recipe, step Step) (Step, error) {
	reached := false
	var untagged []Step
	for _, candidate := range value.Steps {
		if !reached {
			reached = candidate.ID == step.ID
			continue
		}
		if candidate.Effect == "external_write" {
			break
		}
		if candidate.Action != "assert" || candidate.Assert == nil {
			continue
		}
		if candidate.Verifies == step.ID {
			return candidate, nil
		}
		if candidate.Verifies == "" {
			untagged = append(untagged, candidate)
		}
	}
	switch len(untagged) {
	case 1:
		return untagged[0], nil
	case 0:
		return Step{}, fmt.Errorf(
			"external_write step %q has no assert step after it; a local receipt is not proof the remote side committed, so a write must be followed by a step that reads the state back",
			step.ID)
	default:
		return Step{}, fmt.Errorf(
			"external_write step %q is followed by %d untagged assert steps, so nothing says which one reads the write back; tag the declared read-back with verifies %q",
			step.ID, len(untagged), step.ID)
	}
}
