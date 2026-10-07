package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

// SetSiteConsent installs the per-origin consent guard.
func (s *Server) SetSiteConsent(guard *siteconsent.Guard) {
	s.consent = guard
}

func (s *Server) enforceSiteConsent(ctx context.Context, name string, args json.RawMessage) error {
	if !s.consent.Enabled() {
		return nil
	}
	tabID := browser.TabIDFromContext(ctx)
	return s.consent.CheckTool(name, args,
		func(want string) (string, error) { return s.currentPageOrigin(ctx, want) },
		func(ref string) string { return s.refLabels.label(tabID, ref) },
	)
}

func (s *Server) withConsentHooks(ctx context.Context, name string, args json.RawMessage) context.Context {
	if !s.consent.Enabled() && s.approvalGate == nil {
		return ctx
	}
	ctx = browser.WithRuntimeConsent(ctx, s)
	gate := s.consent.NewStepGate(name, args)
	if gate == nil && (s.approvalGate == nil || !siteconsent.SequenceTools[name]) {
		return ctx
	}
	tabID := browser.TabIDFromContext(ctx)
	return browser.WithSequenceGate(ctx, func(index int, stepTabID string, step siteconsent.StepProbe) error {
		stepCtx := browser.WithTabID(ctx, stepTabID)
		raw, _ := json.Marshal(siteconsent.Probe{Steps: []siteconsent.StepProbe{step}})
		if _, err := s.approvalGate.CheckTargets(stepCtx, name, raw); err != nil {
			return err
		}
		return gate.Check(index, step,
			func(want string) (string, error) {
				if want == "" {
					want = stepTabID
				}
				return s.currentPageOrigin(ctx, want)
			},
			func(ref string) string {
				labelTabID := stepTabID
				if labelTabID == "" {
					labelTabID = tabID
				}
				return s.refLabels.label(labelTabID, ref)
			},
		)
	})
}

// CheckFetchDestination gates a URL the DAEMON retrieves itself rather than the page: the one the call named, and every redirect hop after it.
func (s *Server) CheckFetchDestination(rawURL string) error {
	if err := s.approvalGate.CheckURL(rawURL); err != nil {
		return err
	}
	if err := s.checkNavPolicy(rawURL); err != nil {
		return err
	}
	return s.consent.Authorize(rawURL, siteconsent.ScopeRead)
}

// CheckFrameRead gates reaching into one cross-origin iframe.
func (s *Server) CheckFrameRead(frameOrigin string) error {
	if err := s.approvalGate.CheckURL(frameOrigin); err != nil {
		return err
	}
	return s.consent.Authorize(frameOrigin, siteconsent.ScopeRead)
}

// CheckFrameAct gates input into a cross-origin frame against its own act grant.
func (s *Server) CheckFrameAct(frameOrigin string) error {
	if err := s.approvalGate.CheckURL(frameOrigin); err != nil {
		return err
	}
	return s.consent.Authorize(frameOrigin, siteconsent.ScopeAct)
}

func (s *Server) currentPageOrigin(ctx context.Context, want string) (string, error) {
	tabs, err := s.manager.ListTabs(ctx)
	if err != nil {
		return "", fmt.Errorf("site consent needs the tab's current URL to decide, and listing tabs failed: %w", err)
	}
	if want == "" {
		want = browser.TabIDFromContext(ctx)
	}
	for _, tab := range tabs {
		if want != "" && tab.ID == want {
			if err := s.approvalGate.CheckURL(tab.URL); err != nil {
				return "", err
			}
			return tab.URL, nil
		}
	}
	if want != "" {
		return "", fmt.Errorf("site consent cannot decide: tab %s is not open, so there is no origin to check this action against", want)
	}
	if tab, ok := browser.UntargetedTab(ctx, s.manager, tabs); ok {
		if err := s.approvalGate.CheckURL(tab.URL); err != nil {
			return "", err
		}
		return tab.URL, nil
	}
	return "", fmt.Errorf("site consent cannot decide: no active tab, so there is no origin to check this action against")
}

const maxRefLabelsPerTab = 512

const maxRefLabelTabs = 16

type refLabelStore struct {
	mu     sync.Mutex
	byTab  map[string]map[string]string
	recent []string
}

func (r *refLabelStore) record(tabID string, elements []snapshot.Element) {
	if len(elements) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byTab == nil {
		r.byTab = map[string]map[string]string{}
	}
	labels, ok := r.byTab[tabID]
	if !ok {
		labels = map[string]string{}
		r.byTab[tabID] = labels
		r.recent = append(r.recent, tabID)
		for len(r.recent) > maxRefLabelTabs {
			delete(r.byTab, r.recent[0])
			r.recent = r.recent[1:]
		}
	}
	for _, element := range elements {
		if element.Ref == "" || element.Name == "" {
			continue
		}
		if _, known := labels[element.Ref]; !known && len(labels) >= maxRefLabelsPerTab {
			continue
		}
		labels[element.Ref] = element.Name
	}
}

func (r *refLabelStore) label(tabID, ref string) string {
	if ref == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byTab[tabID][ref]
}
