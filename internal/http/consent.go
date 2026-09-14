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

	"github.com/Don-Works/brw/internal/siteconsent"
)

// SetSiteConsent installs the per-origin consent guard. Nil (the default) leaves
// the routes reporting that consent is not configured rather than pretending an
// empty grant list means "nothing is allowed".
func (s *Server) SetSiteConsent(guard *siteconsent.Guard) { s.consent = guard }

// GrantView is one grant as the listing surfaces render it.
//
// Expired is computed here rather than left to each caller: the extension
// options page and the CLI must not each decide what "expired" means, and a
// record that has lapsed but is still listed is exactly what a user needs to see
// to understand why an agent started asking again.
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

// consentCategoryProvenanceView carries where the shipped blocklist came from
// and how to change it, so a user looking at a refused origin can act on it
// without reading the source.
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

// consentMiddleware applies the site-permission gate to the daemon's own HTTP
// routes.
//
// It exists because the MCP server and this API drive the SAME controller. A
// gate on one surface alone is a bypass through the other: `brw open <url>` goes
// straight to /api/browser/open and would never meet an MCP-layer check. The
// rules are the shared table in internal/siteconsent, keyed by the operation
// names usageOperations already maps each route to, so the two surfaces cannot
// drift apart.
//
// POST /dashboard/input is deliberately outside the table. It carries a takeover
// token and dispatches the keystrokes and clicks of a HUMAN who has taken the
// browser over at that moment, and a person at the keyboard IS the consent this
// gate exists to obtain; the token is what proves someone is there. Nothing else
// off /api/ reaches the controller.
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
		// The label lookup is nil here: this surface never returned a snapshot
		// through a per-session cache, so there is no accessible name to recover
		// for a bare ref. Such an action is classified by its origin alone,
		// exactly as an unseen ref is on the MCP surface.
		if err := s.consent.CheckTool(operation, body, func(want string) (string, error) {
			if want == "" {
				want = tabID
			}
			return s.currentPageOrigin(r.Context(), want)
		}, nil); err != nil {
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readConsentBody buffers a request body so the gate can read it and the handler
// behind it still receives every byte.
func readConsentBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Method == http.MethodGet {
		return nil, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		return nil, err
	}
	// The same bytes are replayed, so ContentLength stays true and a handler
	// that decodes strictly behaves exactly as it did.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// currentPageOrigin resolves what the targeted tab is showing. It fails CLOSED
// for the same reason the MCP side does: an action allowed because brw could not
// tell where it was landing is the failure this exists to stop.
func (s *Server) currentPageOrigin(ctx context.Context, tabID string) (string, error) {
	tabs, err := s.manager.ListTabs(ctx)
	if err != nil {
		return "", fmt.Errorf("site consent needs the tab's current URL to decide, and listing tabs failed: %w", err)
	}
	for _, tab := range tabs {
		if tabID != "" && tab.ID == tabID {
			return tab.URL, nil
		}
	}
	if tabID != "" {
		return "", fmt.Errorf("site consent cannot decide: tab %s is not open, so there is no origin to check this action against", tabID)
	}
	for _, tab := range tabs {
		if tab.Active {
			return tab.URL, nil
		}
	}
	return "", errors.New("site consent cannot decide: no active tab, so there is no origin to check this action against")
}
