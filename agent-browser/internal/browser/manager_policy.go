package browser

import (
	"context"

	"github.com/revitt/agent-browser/internal/snapshot"
)

// describeRef resolves the accessible label, href, and current page URL for a
// ref so the policy gate can classify a click before it executes. It is
// best-effort: a resolution failure yields empty label/href (so the gate falls
// back to origin-only evaluation) rather than blocking the action on a lookup
// error. The currentURL is taken from the cached before-state when available to
// avoid an extra round-trip.
func (m *Manager) describeRef(tabCtx context.Context, ref string, before *SemanticState) (label, href, currentURL string) {
	if before != nil {
		currentURL = before.URL
	}
	snap, err := snapshot.Find(tabCtx, snapshot.FindOptions{Query: ref, Limit: 25})
	if err != nil {
		return "", "", currentURL
	}
	if currentURL == "" {
		currentURL = snap.URL
	}
	for _, el := range snap.Elements {
		if el.Ref == ref {
			return el.Name, el.Href, currentURL
		}
	}
	return "", "", currentURL
}

// policyStore returns the manager's policy store, lazily initializing it so a
// zero-value Manager (e.g. constructed directly in a test) still has the safe
// default envelope rather than panicking.
func (m *Manager) policyStore() *PolicyStore {
	m.stateMu.Lock()
	if m.policy == nil {
		m.policy = NewPolicyStore()
	}
	store := m.policy
	m.stateMu.Unlock()
	return store
}

// GetPolicy returns the current session consent/safety envelope.
func (m *Manager) GetPolicy(_ context.Context) (PolicySettings, error) {
	return m.policyStore().Get(), nil
}

// SetPolicy replaces the session consent/safety envelope and returns the stored
// (normalized) settings.
func (m *Manager) SetPolicy(_ context.Context, settings PolicySettings) (PolicySettings, error) {
	return m.policyStore().Set(settings), nil
}

// guardAction evaluates a candidate action against the session policy. When the
// decision blocks the action it returns the wrapped ErrBlockedByPolicy so the
// caller refuses with an explicit error instead of a silent no-op. The returned
// warning string (non-empty only for advisory, non-blocking decisions) should
// be surfaced on the ActionResult.
func (m *Manager) guardAction(currentURL, label, href string) (string, error) {
	decision := EvaluateActionPolicy(m.policyStore().Get(), currentURL, label, href)
	if decision.Blocked {
		return "", decision.Error()
	}
	return decision.Warning, nil
}

// annotateTransition fills the cross-domain transition advisory on a result by
// comparing the page URL before the action with the URL observed after it.
func annotateTransition(result *ActionResult, beforeURL string) {
	if result == nil {
		return
	}
	if domain, crossed := CrossDomainTransition(beforeURL, result.URL); crossed {
		result.DomainTransition = domain
	}
}
