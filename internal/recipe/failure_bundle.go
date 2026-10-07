package recipe

import (
	"context"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
)

// FailureEvidence is an optional Surface capability: collecting, in one pass on the page that has just failed, everything needed to diagnose the failure without running the recipe again.
type FailureEvidence interface {
	CaptureFailureBundle(context.Context, FailureBundleRequest) (artifact.Meta, error)
}

// FailureBundleRequest carries what the manifest records about the failure.
type FailureBundleRequest struct {
	RecipeID      string
	RecipeVersion string
	FailedStep    string
	Reason        string
	OptIn         bool
}

const failureEvidenceBudget = 20 * time.Second

func (r Runner) captureFailureEvidence(ctx context.Context, value Recipe, stepID, reason string) string {
	evidence, ok := r.Surface.(FailureEvidence)
	if !ok {
		return ""
	}

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

// CaptureFailureBundle bridges the deterministic surface to the browser-host artifact service.
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
