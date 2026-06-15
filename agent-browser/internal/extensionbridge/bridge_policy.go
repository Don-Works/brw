package extensionbridge

import (
	"context"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// policyStore returns the bridge's policy store, lazily initializing it so a
// zero-value Bridge still carries the safe default consent envelope.
func (b *Bridge) policyStore() *browser.PolicyStore {
	b.mu.Lock()
	if b.policy == nil {
		b.policy = browser.NewPolicyStore()
	}
	store := b.policy
	b.mu.Unlock()
	return store
}

// GetPolicy returns the current session consent/safety envelope.
func (b *Bridge) GetPolicy(_ context.Context) (browser.PolicySettings, error) {
	return b.policyStore().Get(), nil
}

// SetPolicy replaces the session consent/safety envelope and returns the stored
// (normalized) settings.
func (b *Bridge) SetPolicy(_ context.Context, settings browser.PolicySettings) (browser.PolicySettings, error) {
	return b.policyStore().Set(settings), nil
}

// guardAction evaluates a candidate action against the bridge's session policy.
// A blocking decision yields the wrapped ErrBlockedByPolicy; otherwise the
// (possibly empty) advisory warning is returned.
func (b *Bridge) guardAction(currentURL, label, href string) (string, error) {
	decision := browser.EvaluateActionPolicy(b.policyStore().Get(), currentURL, label, href)
	if decision.Blocked {
		return "", decision.Error()
	}
	return decision.Warning, nil
}

// describeRef resolves the accessible label, href, and current page URL for a
// ref so the policy gate can classify a click before it executes. Best-effort:
// a resolution failure yields empty label/href rather than blocking on a lookup
// error.
func (b *Bridge) describeRef(ctx context.Context, ref string, before *browser.SemanticState) (label, href, currentURL string) {
	if before != nil {
		currentURL = before.URL
	}
	found, err := b.Find(ctx, snapshot.FindOptions{Query: ref, Limit: 25})
	if err != nil {
		return "", "", currentURL
	}
	if currentURL == "" {
		currentURL = found.URL
	}
	for _, el := range found.Elements {
		if el.Ref == ref {
			return el.Name, el.Href, currentURL
		}
	}
	return "", "", currentURL
}

// annotateBridgeTransition fills the cross-domain transition advisory on a
// result by comparing the page URL before the action with the URL observed
// after it.
func annotateBridgeTransition(result *browser.ActionResult, beforeURL string) {
	if result == nil {
		return
	}
	if domain, crossed := browser.CrossDomainTransition(beforeURL, result.URL); crossed {
		result.DomainTransition = domain
	}
}
