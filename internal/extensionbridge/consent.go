package extensionbridge

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// SetSiteConsent installs the per-origin consent guard the extension's options
// page reads and revokes through. Nil leaves the routes answering
// "not configured".
func (b *Bridge) SetSiteConsent(guard *siteconsent.Guard) { b.consent = guard }

// consentGrantView is one record as the options page renders it.
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

// handleConsent serves the grant list to the extension's options page.
//
// Same reachability rule as the handshake token, and the same method, so the
// two cannot drift: loopback Host, an initiator that is not another document,
// and either no Origin or the configured extension's own Origin. An MV3
// worker's fetch sends no Origin and Sec-Fetch-Site: none
// (measured on Chromium 152.0.7977.82 on 2026-09-15; see docs/auth-model.md),
// and a web page satisfies neither branch, so a site the user is visiting
// cannot read which other sites they have granted - which is itself a small
// profile of the user.
func (b *Bridge) handleConsent(w http.ResponseWriter, r *http.Request) {
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

// handleConsentRevoke revokes one grant, or all of them, from the options page.
func (b *Bridge) handleConsentRevoke(w http.ResponseWriter, r *http.Request) {
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
	if !b.consent.Enabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site consent is not configured on this daemon"})
		return
	}
	// The actor is recorded as the options page rather than the OS account: this
	// request arrives over a socket and brw cannot verify who is at the keyboard,
	// so the ledger says where the revocation came in rather than claiming a
	// person it did not authenticate.
	const actor = "extension-options-page"
	if req.All {
		if req.Origin != "" {
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
