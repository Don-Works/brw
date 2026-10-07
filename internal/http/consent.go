package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// SetSiteConsent installs the per-origin consent guard.
func (s *Server) SetSiteConsent(guard *siteconsent.Guard) { s.consent = guard }

// GrantView is one grant as the listing surfaces render it.
type GrantView struct {
	siteconsent.Grant
	Expired bool `json:"expired"`
}

type consentListResponse struct {
	Enabled  bool                          `json:"enabled"`
	Path     string                        `json:"path,omitempty"`
	Grants   []GrantView                   `json:"grants"`
	Rejected []siteconsent.RejectedRecord  `json:"rejected"`
	Category consentCategoryProvenanceView `json:"categories"`
}

type consentCategoryProvenanceView struct {
	Version  string   `json:"version"`
	Source   string   `json:"source"`
	Update   string   `json:"update"`
	Coverage string   `json:"coverage"`
	Names    []string `json:"names"`
}

var errConsentNotConfigured = errors.New("site consent is not configured on this daemon; start brwd with --site-consent to enable it")

func (s *Server) consentGrants(w http.ResponseWriter, _ *http.Request) {
	if !s.consent.Enabled() {
		writeJSON(w, http.StatusOK, consentListResponse{Grants: []GrantView{}, Rejected: []siteconsent.RejectedRecord{}})
		return
	}
	store := s.consent.Store()
	categories := s.consent.Categories()
	names := make([]string, 0, len(categories.Categories))
	for _, category := range categories.Categories {
		names = append(names, category.Name)
	}
	now := time.Now()
	grants := store.List()
	views := make([]GrantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, GrantView{Grant: grant, Expired: grant.Expired(now)})
	}
	rejected := store.Rejected()
	if rejected == nil {
		rejected = []siteconsent.RejectedRecord{}
	}
	writeJSON(w, http.StatusOK, consentListResponse{
		Enabled:  true,
		Path:     store.Path(),
		Grants:   views,
		Rejected: rejected,
		Category: consentCategoryProvenanceView{
			Version:  categories.Version,
			Source:   categories.Source,
			Update:   categories.Update,
			Coverage: categories.Coverage,
			Names:    names,
		},
	})
}

func (s *Server) consentRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Origin string `json:"origin"`
		Scope  string `json:"scope"`
		All    bool   `json:"all"`
		Actor  string `json:"actor"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if !s.consent.Enabled() {
		writeError(w, errConsentNotConfigured)
		return
	}
	if req.All {
		if req.Origin != "" {
			writeError(w, errors.New("revoke takes either all or an origin, not both"))
			return
		}
		removed, err := s.consent.RevokeAll(req.Actor)
		writeResult(w, map[string]any{"ok": err == nil, "removed": removed}, err)
		return
	}
	if req.Origin == "" {
		writeError(w, errors.New("revoke requires an origin, or all"))
		return
	}
	var scope siteconsent.Scope
	if req.Scope != "" {
		parsed, err := siteconsent.ParseScope(req.Scope)
		if err != nil {
			writeError(w, err)
			return
		}
		scope = parsed
	}
	removed, err := s.consent.Revoke(req.Origin, scope, req.Actor)
	writeResult(w, map[string]any{"ok": err == nil, "removed": removed}, err)
}

func (s *Server) consentMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.consent.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		operation := usageOperations[r.URL.Path]
		_, gated := siteconsent.ToolRules[operation]
		if !gated && !siteconsent.SequenceTools[operation] {
			next.ServeHTTP(w, r)
			return
		}
		body, err := readConsentBody(w, r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		tabID := r.URL.Query().Get("tab_id")
		if tabID == "" {
			var probe struct {
				TabID string `json:"tab_id"`
			}
			_ = json.Unmarshal(body, &probe)
			tabID = probe.TabID
		}

		if err := s.consent.CheckTool(operation, body, func(want string) (string, error) {
			if want == "" {
				want = tabID
			}
			return s.currentPageOrigin(r.Context(), want)
		}, nil); err != nil {
			var confirmation *siteconsent.ConfirmationRequiredError
			if s.approvalGate == nil || !errors.As(err, &confirmation) {
				writeError(w, err)
				return
			}
			r = r.WithContext(markApprovalRequired(r.Context()))
		}

		next.ServeHTTP(w, r.WithContext(s.withConsentHooks(r.Context(), operation, body, tabID)))
	})
}

func (s *Server) withConsentHooks(ctx context.Context, operation string, body []byte, tabID string) context.Context {
	if !s.consent.Enabled() && s.approvalGate == nil {
		return ctx
	}
	ctx = browser.WithRuntimeConsent(ctx, s)
	gate := s.consent.NewStepGate(operation, body)
	if gate == nil && (s.approvalGate == nil || !siteconsent.SequenceTools[operation]) {
		return ctx
	}
	return browser.WithSequenceGate(ctx, func(index int, stepTabID string, step siteconsent.StepProbe) error {
		stepCtx := browser.WithTabID(ctx, stepTabID)
		raw, _ := json.Marshal(siteconsent.Probe{Steps: []siteconsent.StepProbe{step}})
		if _, err := s.approvalGate.CheckTargets(stepCtx, operation, raw); err != nil {
			return err
		}

		return gate.Check(index, step, func(want string) (string, error) {
			if want == "" {
				want = stepTabID
			}
			if want == "" {
				want = tabID
			}
			return s.currentPageOrigin(ctx, want)
		}, nil)
	})
}

// CheckFetchDestination gates a URL the daemon retrieves itself, on the call's own URL and on every redirect hop after it.
func (s *Server) CheckFetchDestination(rawURL string) error {
	if err := s.approvalGate.CheckURL(rawURL); err != nil {
		return err
	}
	if err := s.checkNavPolicy(rawURL); err != nil {
		return err
	}
	return s.consent.Authorize(rawURL, siteconsent.ScopeRead)
}

// CheckFrameRead gates reaching into one cross-origin iframe, against that frame's own origin rather than the embedder's.
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

func readConsentBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Method == http.MethodGet {
		return nil, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		return nil, err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func (s *Server) currentPageOrigin(ctx context.Context, tabID string) (string, error) {
	tabs, err := s.manager.ListTabs(ctx)
	if err != nil {
		return "", fmt.Errorf("site consent needs the tab's current URL to decide, and listing tabs failed: %w", err)
	}
	for _, tab := range tabs {
		if tabID != "" && tab.ID == tabID {
			if err := s.approvalGate.CheckURL(tab.URL); err != nil {
				return "", err
			}
			return tab.URL, nil
		}
	}
	if tabID != "" {
		return "", fmt.Errorf("site consent cannot decide: tab %s is not open, so there is no origin to check this action against", tabID)
	}
	if tab, ok := browser.UntargetedTab(ctx, s.manager, tabs); ok {
		if err := s.approvalGate.CheckURL(tab.URL); err != nil {
			return "", err
		}
		return tab.URL, nil
	}
	return "", errors.New("site consent cannot decide: no active tab, so there is no origin to check this action against")
}
