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

// withConsentHooks installs the two consent checks that cannot be answered
// before dispatch: the per-step re-check a plan or batch needs once its own
// steps start moving the page, and the fetch check a daemon-side retrieval needs
// once a server answers with a redirect.
//
// Both are no-ops on a daemon with no consent store, so a controller reached
// through this path behaves exactly as it did before consent existed.
func (s *Server) withConsentHooks(ctx context.Context, name string, args json.RawMessage) context.Context {
	if !s.consent.Enabled() {
		return ctx
	}
	ctx = browser.WithFetchCheck(ctx, s.checkFetchDestination)
	ctx = browser.WithFrameReadCheck(ctx, s.checkFrameRead)
	gate := s.consent.NewStepGate(name, args)
	if gate == nil {
		return ctx
	}
	tabID := browser.TabIDFromContext(ctx)
	return browser.WithSequenceGate(ctx, func(index int, stepTabID string, step siteconsent.StepProbe) error {
		return gate.Check(index, step,
			func(want string) (string, error) {
				if want == "" {
					want = stepTabID
				}
				return s.currentPageOrigin(ctx, want)
			},
			func(ref string) string {
				if label := s.refLabels.label(stepTabID, ref); label != "" {
					return label
				}
				return s.refLabels.label(tabID, ref)
			},
		)
	})
}

// checkFetchDestination gates a URL the DAEMON retrieves itself rather than the
// page: the one the call named, and every redirect hop after it.
//
// A grant is for an origin, not for a request. Gating only the first URL made a
// 302 from a granted site into a read of whatever it pointed at, which is the
// document brw never asked for that the read scope exists to cover.
func (s *Server) checkFetchDestination(rawURL string) error {
	if err := s.checkNavPolicy(rawURL); err != nil {
		return err
	}
	return s.consent.Authorize(rawURL, siteconsent.ScopeRead)
}

// checkFrameRead gates reading the document inside one cross-origin iframe.
//
// brw_snapshot is gated against the origin the TAB is showing. include_frames
// then attaches a session to each embedded frame's own target and runs the walker
// in a third party's document — a read of that third party, which the embedder's
// grant does not cover and which the tool's arguments never named.
func (s *Server) checkFrameRead(frameOrigin string) error {
	return s.consent.Authorize(frameOrigin, siteconsent.ScopeRead)
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
			return tab.URL, nil
		}
	}
	if want != "" {
		return "", fmt.Errorf("site consent cannot decide: tab %s is not open, so there is no origin to check this action against", want)
	}
	for _, tab := range tabs {
		if tab.Active {
			return tab.URL, nil
		}
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
		if len(labels) >= maxRefLabelsPerTab {
			break
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
	if label := r.byTab[tabID][ref]; label != "" {
		return label
	}
	// A call with no tab_id resolves the active tab downstream, so the label may
	// be filed under a tab id this call never saw. Refs are unique per page and
	// a wrong label can only add a confirmation prompt, never remove one.
	for _, labels := range r.byTab {
		if label := labels[ref]; label != "" {
			return label
		}
	}
	return ""
}
