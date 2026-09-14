package recipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// NonceReader is an optional Surface capability: reading the value of the field
// a step declared as the site's own idempotency token.
//
// Optional so a Surface written against the previous ABI still compiles, and
// named in the error so a missing capability is reported rather than worked
// around. A step that declares a nonce never runs without one: falling back to
// brw's derived key would swap the site's mechanism for the weaker one at the
// exact moment the recipe asked for the site's.
type NonceReader interface {
	ElementValue(context.Context, Target) (string, error)
}

// checkReceiptCapabilities refuses a recipe whose write mechanism this runner
// cannot honour, before any step executes.
//
// A step declaring a site nonce is declaring which mechanism protects it. Run
// it without a receipt store and the declaration does nothing at all: the nonce
// is never read, never recorded, and never compared on a rerun. That is a
// silent degrade from the strong mechanism to none, so it is named instead.
func (r Runner) checkReceiptCapabilities(value Recipe) error {
	if r.Receipts != nil {
		return nil
	}
	for _, step := range value.Steps {
		if step.SiteIdempotency != nil {
			return fmt.Errorf("step %q declares a site idempotency nonce, but this runner has no write receipt store to record it against", step.ID)
		}
	}
	return nil
}

// writeReceipt carries one external write's provider-side record through the
// dispatch it protects.
type writeReceipt struct {
	receipts Receipts
	key      string
	// resolved reports that no browser write should be issued: either the
	// provider already holds a committed receipt, or an interrupted earlier
	// attempt was confirmed by re-reading remote state.
	resolved bool
	evidence string
}

func (w *writeReceipt) commit(ctx context.Context) error {
	_, err := w.receipts.Commit(ctx, w.key, w.evidence)
	return err
}

// openWriteReceipt records this write with the provider before anything is
// dispatched, and decides what to do when a record already exists.
//
// The caller has already preflighted the postcondition and found it unmet, so
// arriving here means the desired state is not present. Three cases follow: the
// provider has no record and this is a first attempt; the provider has a
// committed record, so the write is done and dispatching would duplicate it;
// or the provider has an in-flight record, which is what a daemon killed
// between dispatch and acknowledgement leaves behind. Only the last needs
// judgement, and the judgement is never "try again and see".
func (r Runner) openWriteReceipt(ctx context.Context, value Recipe, step Step, inputs map[string]string) (*writeReceipt, error) {
	origin, err := r.Surface.Origin(ctx)
	if err != nil {
		return nil, err
	}
	key, err := ReceiptKey(value, step, origin, inputs)
	if err != nil {
		return nil, err
	}
	digest, err := Digest(value)
	if err != nil {
		return nil, err
	}
	nonce, err := r.readSiteNonce(ctx, step, inputs)
	if err != nil {
		return nil, err
	}
	pending := &writeReceipt{receipts: r.Receipts, key: key}

	existing, found, err := r.Receipts.Lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read write receipt: %w", err)
	}
	if found {
		if err := receiptMatchesWrite(existing, key, value, step, origin); err != nil {
			return nil, err
		}
		switch existing.Status {
		case ReceiptCommitted:
			pending.resolved = true
			return pending, nil
		case ReceiptInFlight:
			verified, detail := r.verifyInterruptedWrite(ctx, value, step, inputs)
			if verified {
				pending.evidence = "verification step " + detail + " passed after an interrupted dispatch"
				if err := pending.commit(ctx); err != nil {
					return nil, err
				}
				pending.resolved = true
				return pending, nil
			}
			if nonce != "" && existing.SiteNonce == nonce {
				// The page still holds the token the interrupted attempt
				// carried, and the recipe declares that the site rejects a
				// second submission bearing it. brw defers to that mechanism
				// rather than to its own, which is what declaring it means.
				return pending, nil
			}
			return nil, fmt.Errorf(
				"step %q was dispatched by an earlier run and its receipt is still in flight; re-reading remote state did not confirm it (%s), so brw refuses to re-submit an external write whose outcome is unknown",
				step.ID, detail)
		}
	}

	begun, err := r.Receipts.Begin(ctx, Receipt{
		Key: key, RecipeID: value.ID, RecipeVersion: value.Version, RecipeDigest: digest,
		StepID: step.ID, Origin: origin, Status: ReceiptInFlight, SiteNonce: nonce,
	})
	if err != nil {
		return nil, fmt.Errorf("record write receipt before dispatch: %w", err)
	}
	if begun.Key != key {
		return nil, errors.New("write receipt store returned a receipt for a different key")
	}
	return pending, nil
}

// receiptMatchesWrite refuses a receipt that does not describe this write. The
// key is a hash, and a store that answers a lookup with somebody else's record
// would otherwise suppress a write that had never been dispatched.
func receiptMatchesWrite(receipt Receipt, key string, value Recipe, step Step, origin string) error {
	exactOrigin, err := normalizeReceiptOrigin(origin)
	if err != nil {
		return err
	}
	storedOrigin, err := normalizeReceiptOrigin(receipt.Origin)
	if err != nil {
		return fmt.Errorf("write receipt store returned an unusable origin: %w", err)
	}
	if receipt.Key != key || receipt.RecipeID != value.ID || receipt.RecipeVersion != value.Version ||
		receipt.StepID != step.ID || storedOrigin != exactOrigin {
		return errors.New("write receipt store returned a receipt describing a different write")
	}
	if receipt.Status != ReceiptInFlight && receipt.Status != ReceiptCommitted {
		return fmt.Errorf("write receipt store returned unsupported status %q", receipt.Status)
	}
	return nil
}

// verifyInterruptedWrite runs the recipe's declared verification step against
// live remote state. A failed assertion and a transport failure are treated the
// same way — neither is confirmation — and the detail is carried into the
// refusal so an operator sees which it was.
func (r Runner) verifyInterruptedWrite(ctx context.Context, value Recipe, step Step, inputs map[string]string) (bool, string) {
	verification, ok := verificationStepFor(value, step)
	if !ok {
		return false, "the recipe declares no verification step"
	}
	asserter, ok := r.Surface.(Asserter)
	if !ok {
		return false, "this browser surface provides no deterministic assertions"
	}
	assertion, err := expandAssertion(*verification.Assert, inputs)
	if err != nil {
		return false, "verification step " + verification.ID + " could not be expanded: " + redactInputs(err, inputs).Error()
	}
	if err := asserter.Assert(ctx, assertion); err != nil {
		return false, "verification step " + verification.ID + " did not pass: " + redactInputs(err, inputs).Error()
	}
	return true, verification.ID
}

// verificationStepFor finds the assertion that reads back what a write did: the
// first assert step after it, stopping at the next write because that assert
// belongs to the next one.
func verificationStepFor(value Recipe, step Step) (Step, bool) {
	reached := false
	for _, candidate := range value.Steps {
		if !reached {
			reached = candidate.ID == step.ID
			continue
		}
		if candidate.Effect == "external_write" {
			return Step{}, false
		}
		if candidate.Action == "assert" && candidate.Assert != nil {
			return candidate, true
		}
	}
	return Step{}, false
}

// readSiteNonce reads the site's own per-submission token, when the step
// declares one.
func (r Runner) readSiteNonce(ctx context.Context, step Step, inputs map[string]string) (string, error) {
	if step.SiteIdempotency == nil {
		return "", nil
	}
	reader, ok := r.Surface.(NonceReader)
	if !ok {
		return "", fmt.Errorf("step %q declares a site idempotency nonce, which this browser surface cannot read", step.ID)
	}
	target, err := expandTarget(*step.SiteIdempotency.Target, inputs)
	if err != nil {
		return "", err
	}
	nonce, err := reader.ElementValue(ctx, target)
	if err != nil {
		return "", fmt.Errorf("read site idempotency nonce for step %q: %w", step.ID, redactInputs(err, inputs))
	}
	if strings.TrimSpace(nonce) == "" {
		return "", fmt.Errorf("step %q declares a site idempotency nonce but the field is empty; refusing to dispatch a write without the token the site deduplicates on", step.ID)
	}
	return nonce, nil
}
