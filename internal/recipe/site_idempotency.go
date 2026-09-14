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
// When a step declares one, brw prefers it over the key brw derives itself,
// because the site is the party that will reject the duplicate; brw's own key
// only stops brw from asking twice. A declared nonce that cannot be read is
// fatal to the step rather than a reason to fall back — falling back would
// quietly replace the strong mechanism with the weaker one at the moment it
// matters.
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

// RequireWriteVerification refuses a recipe whose external write is not
// followed by an assertion.
//
// A receipt records that brw dispatched a write; it is written by brw, on brw's
// side of the network, and a process that dies mid-request writes exactly the
// same receipt whether the remote side committed or not. The only thing that
// can answer "did it commit" is reading the state back, so a recipe that writes
// has to contain that read.
func RequireWriteVerification(value Recipe) error {
	var problems []error
	for index, step := range value.Steps {
		if step.Effect != "external_write" {
			continue
		}
		verified := false
		for _, later := range value.Steps[index+1:] {
			if later.Effect == "external_write" {
				break
			}
			if later.Action == "assert" {
				verified = true
				break
			}
		}
		if !verified {
			problems = append(problems, fmt.Errorf(
				"external_write step %q has no assert step after it; a local receipt is not proof the remote side committed, so a write must be followed by a step that reads the state back",
				step.ID))
		}
	}
	return errors.Join(problems...)
}
