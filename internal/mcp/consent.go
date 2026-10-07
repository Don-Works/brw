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

// SetSiteConsent installs the per-origin consent guard. A nil guard, or one
// built without a store, leaves every tool exactly as it was: consent is opt-in
// and a daemon started without it must not behave differently.
func (s *Server) SetSiteConsent(guard *siteconsent.Guard) {
	s.consent = guard
}

// enforceSiteConsent is the gate every tool call passes through.
//
// The rules themselves live in internal/siteconsent because the HTTP API serves
// the same controller: a gate enforced here alone would be bypassable by calling
// the daemon's own route instead. This function is only the MCP-side plumbing -
// where the live origin comes from, and where a ref's label comes from.
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

// withConsentHooks installs the consent checks that cannot be answered before
// dispatch: the per-step re-check a plan or batch needs once its own steps start
// moving the page, the fetch check a daemon-side retrieval needs once a server
// answers with a redirect, and the frame-read check a cross-origin iframe needs
// once the page has been walked and its embedded origins are known.
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

// CheckFetchDestination gates a URL the DAEMON retrieves itself rather than the
// page: the one the call named, and every redirect hop after it.
//
// A grant is for an origin, not for a request. Gating only the first URL made a
// 302 from a granted site into a read of whatever it pointed at, which is the
// document brw never asked for that the read scope exists to cover.
//
// It is exported because it is half of browser.ConsentEnforcer, which is what
// makes "every runtime question is answered" a compile error rather than a habit.
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

// currentPageOrigin resolves the origin a tab is showing. An empty want is the
// tab this call targets; a named one is a tab the call moves to, which a plan's
// focus_tab step does mid-sequence.
//
// It fails CLOSED. If the transport cannot say what the tab is showing, consent
// cannot be checked against anything, and an action allowed because brw did not
// know where it was landing is the failure this whole surface exists to stop.
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

// maxRefLabelsPerTab bounds the ref-to-label memory. A page with more elements
// than this loses its oldest entries, which costs a classification signal and
// nothing else.
const maxRefLabelsPerTab = 512

// maxRefLabelTabs bounds how many tabs are remembered at once.
const maxRefLabelTabs = 16

// refLabelStore remembers the accessible name brw already reported for a ref, so
// the confirmation gate can classify a click addressed by ref.
//
// Nothing here is a source of truth about the page: it is what brw last told the
// agent, which is exactly the label the agent acted on. A ref this has never
// seen simply yields no label, and the action is then classified by its origin
// alone.
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
