package recipe

import (
	"context"
	"fmt"

	"github.com/Don-Works/brw/internal/credential"
)

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

func (r Runner) actuateFromCredential(ctx context.Context, action, elementRef, reference string) error {
	if r.Credentials == nil {
		return fmt.Errorf("value is %s%s: %w", credential.Scheme, reference, credential.ErrNoProvider)
	}
	secret, err := r.Credentials.Resolve(ctx, reference)
	if err != nil {

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

		return fmt.Errorf("credential references are not allowed on action %q", action)
	}

	return credential.Scrub(actionErr, secret)
}
