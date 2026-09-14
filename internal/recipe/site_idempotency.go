package recipe

import (
	"errors"
	"fmt"
)

// SiteIdempotencyFormNonce is the one mechanism brw can actually use: a token
// the page mints per submission and carries in a form field, which the site
// checks when the submission arrives.
//
// Deliberately the only kind. A site that accepts an Idempotency-Key request
// header is doing the same job, but a recipe drives a page, not an HTTP client:
// brw cannot attach a header to the request a form submit issues, so declaring
// one would promise a mechanism that never runs.
const SiteIdempotencyFormNonce = "form_nonce"

// SiteIdempotency names the target site's own duplicate-suppression token.
//
// It is recorded, never acted on. A page still showing the token an interrupted
// attempt carried is consistent with the site having consumed it, and equally
// consistent with the token being a per-session CSRF value the site will accept
// twice; brw cannot tell those apart from outside, so an unchanged token is
// never grounds to dispatch again. What the declaration buys is evidence — the
// receipt carries a digest of the token that was in flight — and a step whose
// declared token cannot be read does not run at all, because a flow that leans
// on the site's mechanism must not proceed when that mechanism is absent.
type SiteIdempotency struct {
	Kind string `json:"kind"`
	// Target names the field carrying the token. It is a semantic target like
	// any other, so the nonce field is found the same way every element is.
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

// RequireWriteVerification refuses a recipe whose external write has no
// unambiguous read-back.
//
// A receipt records that brw dispatched a write; it is written by brw, on brw's
// side of the network, and a process that dies mid-request writes exactly the
// same receipt whether the remote side committed or not. The only thing that
// can answer "did it commit" is reading the state back, so a recipe that writes
// has to contain that read — and has to say which assertion it is, because a
// rerun that consults the wrong one commits a receipt for a write that never
// landed.
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
//
// A step whose Verifies names the write is the answer whenever one exists: the
// compiler emits an inferred evidence assertion as well as the operator's
// declared read-back, and which of the two comes first is an emission detail,
// not a statement about which one proves the write landed. Without a tag, a
// single assertion between this write and the next is unambiguous and is
// accepted, so a recipe hand-authored against the earlier shape keeps both its
// meaning and its digest. Several untagged assertions are refused rather than
// guessed between.
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
