package extensionbridge

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// SetSiteConsent installs the per-origin consent guard the extension's options page reads and revokes through.
func (b *Bridge) SetSiteConsent(guard *siteconsent.Guard) { b.consent = guard }

type consentGrantView struct {
	siteconsent.Grant
	Expired bool `json:"expired"`
}

type consentStatus struct {
	Enabled  bool                         `json:"enabled"`
	Grants   []consentGrantView           `json:"grants"`
	Rejected []siteconsent.RejectedRecord `json:"rejected"`
	Source   string                       `json:"category_source,omitempty"`
	Update   string                       `json:"category_update,omitempty"`
	Version  string                       `json:"category_version,omitempty"`
}

func (b *Bridge) handleConsent(w http.ResponseWriter, r *http.Request) {
	markInitiatorSensitive(w)
	if !b.tokenServable(r) {
		http.Error(w, "consent surface is served only to the local extension", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.consent.Enabled() {
		writeJSON(w, http.StatusOK, consentStatus{Grants: []consentGrantView{}, Rejected: []siteconsent.RejectedRecord{}})
		return
	}
	store := b.consent.Store()
	now := time.Now()
	grants := store.List()
	views := make([]consentGrantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, consentGrantView{Grant: grant, Expired: grant.Expired(now)})
	}
	rejected := store.Rejected()
	if rejected == nil {
		rejected = []siteconsent.RejectedRecord{}
	}
	categories := b.consent.Categories()
	writeJSON(w, http.StatusOK, consentStatus{
		Enabled: true, Grants: views, Rejected: rejected,
		Source: categories.Source, Update: categories.Update, Version: categories.Version,
	})
}

func (b *Bridge) handleConsentRevoke(w http.ResponseWriter, r *http.Request) {
	markInitiatorSensitive(w)
	if !b.tokenServable(r) {
		http.Error(w, "consent surface is served only to the local extension", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Origin string `json:"origin"`
		Scope  string `json:"scope"`
		All    bool   `json:"all"`
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revoke body must contain one JSON object"})
		return
	}
	if !b.consent.Enabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site consent is not configured on this daemon"})
		return
	}

	const actor = "extension-options-page"
	if req.All {
		if req.Origin != "" || req.Scope != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revoke takes either all or an origin, not both"})
			return
		}
		removed, err := b.consent.RevokeAll(actor)
		writeConsentResult(w, removed, err)
		return
	}
	if req.Origin == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revoke requires an origin, or all"})
		return
	}
	var scope siteconsent.Scope
	if req.Scope != "" {
		parsed, err := siteconsent.ParseScope(req.Scope)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		scope = parsed
	}
	removed, err := b.consent.Revoke(req.Origin, scope, actor)
	writeConsentResult(w, removed, err)
}

func writeConsentResult(w http.ResponseWriter, removed int, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}
