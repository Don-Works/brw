package recipe

import (
	"context"
	"fmt"

	"github.com/Don-Works/brw/internal/credential"
)

// preflightCredentials fails a run that names a credential nothing can resolve,
// before the first browser action. Discovering it at the password field would
// leave a half-filled login form and a session the recipe cannot finish.
func preflightCredentials(value Recipe, resolver credential.Resolver) error {
	for index, step := range value.Steps {
		reference, ok := StepCredentialReference(step)
		if !ok {
			continue
		}
		if err := credential.Probe(resolver); err != nil {
			return fmt.Errorf("step %d %q references %s%s: %w", index+1, step.ID, credential.Scheme, reference, err)
		}
	}
	return nil
}

// actuateFromCredential resolves the reference at dispatch and hands the value
// to exactly one browser actuation.
//
// Three things happen here and nowhere else in brw: the provider is called, the
// value is revealed, and the value is wiped. Nothing in between stores it —
// there is no cache, no step result field, and no map keyed by reference — so a
// walk of the daemon's retained state after this returns finds nothing.
func (r Runner) actuateFromCredential(ctx context.Context, action, elementRef, reference string) error {
	if r.Credentials == nil {
		return fmt.Errorf("value is %s%s: %w", credential.Scheme, reference, credential.ErrNoProvider)
	}
	secret, err := r.Credentials.Resolve(ctx, reference)
	if err != nil {
		// The reference NAME is safe to report and is the only thing an operator
		// can act on; the value is not in this error because it was never
		// produced.
		return fmt.Errorf("resolve credential %q: %w", reference, err)
	}
	defer secret.Wipe()
	if secret.Empty() {
		return fmt.Errorf("resolve credential %q: %w", reference, credential.ErrEmptyValue)
	}
	var actionErr error
	switch action {
	case "fill":
		actionErr = r.Surface.Fill(ctx, elementRef, secret.Reveal())
	case "type":
		actionErr = r.Surface.Type(ctx, elementRef, secret.Reveal())
	default:
		// Unreachable through Validate, which allows a reference only on the
		// actions above. Kept so adding an action to CredentialActions without
		// adding a branch here fails closed rather than typing nothing.
		return fmt.Errorf("credential references are not allowed on action %q", action)
	}
	// Scrub before the error leaves this function: a transport that quotes the
	// text it failed to type is the realistic path from a failed fill into the
	// run result, the MCP response and the failure bundle's reason.
	return credential.Scrub(actionErr, secret)
}
