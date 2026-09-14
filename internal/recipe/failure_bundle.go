package recipe

import (
	"context"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
)

// FailureEvidence is an optional Surface capability: collecting, in one pass on
// the page that has just failed, everything needed to diagnose the failure
// without running the recipe again. A surface that cannot do it is not an
// error; the run still reports the failure it always reported.
type FailureEvidence interface {
	CaptureFailureBundle(context.Context, FailureBundleRequest) (artifact.Meta, error)
}

// FailureBundleRequest carries what the manifest records about the failure.
// OptIn is the recipe's own declaration; whether it is honoured is the browser
// host's decision, not the recipe's.
type FailureBundleRequest struct {
	RecipeID      string
	RecipeVersion string
	FailedStep    string
	Reason        string
	OptIn         bool
}

// failureEvidenceBudget bounds the collection. It is a handful of browser round
// trips against a page that has just misbehaved, so it needs a ceiling of its
// own — and it must not inherit the run deadline, because the most common
// reason to want evidence is that the run ran out of time.
const failureEvidenceBudget = 20 * time.Second

// captureFailureEvidence returns the manifest artifact id, or "" when no
// evidence was collected. Every failure mode here is silent by design: the
// caller already has a real error to report, and replacing it with "could not
// collect evidence" would hide the thing that actually went wrong.
func (r Runner) captureFailureEvidence(ctx context.Context, value Recipe, stepID, reason string) string {
	evidence, ok := r.Surface.(FailureEvidence)
	if !ok {
		return ""
	}
	// WithoutCancel, then a fresh budget: a cancelled or expired run context is
	// exactly the case this feature exists for. WithAllowedOrigins is reapplied
	// because the collection still captures page bytes and must stay inside the
	// same reviewed origin boundary the run itself had.
	bundleCtx, cancel := context.WithTimeout(
		browser.WithAllowedOrigins(context.WithoutCancel(ctx), value.Origins), failureEvidenceBudget)
	defer cancel()
	meta, err := evidence.CaptureFailureBundle(bundleCtx, FailureBundleRequest{
		RecipeID: value.ID, RecipeVersion: value.Version, FailedStep: stepID,
		Reason: reason, OptIn: value.CaptureOnFailure,
	})
	if err != nil {
		return ""
	}
	return meta.ID
}

// CaptureFailureBundle bridges the deterministic surface to the browser-host
// artifact service. The service owns the policy: this only reports what the
// recipe asked for.
func (s *BrowserSurface) CaptureFailureBundle(ctx context.Context, req FailureBundleRequest) (artifact.Meta, error) {
	if s.Artifacts == nil {
		return artifact.Meta{}, errors.New("artifact service is not configured")
	}
	bundler, ok := s.Artifacts.(interface {
		CaptureFailureBundle(context.Context, artifact.FailureBundleOptions) (artifact.Meta, error)
	})
	if !ok {
		return artifact.Meta{}, errors.New("this artifact service cannot collect failure evidence")
	}
	return bundler.CaptureFailureBundle(ctx, artifact.FailureBundleOptions{
		Reason:        req.Reason,
		RecipeID:      req.RecipeID,
		RecipeVersion: req.RecipeVersion,
		FailedStep:    req.FailedStep,
		RecipeOptIn:   req.OptIn,
	})
}
