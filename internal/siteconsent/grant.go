package siteconsent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Scope is what a grant permits on an origin.
//
// Two levels, not a matrix of verbs: a user can hold "brw may look at this site"
// and "brw may change things on this site" in their head, and a longer list is a
// list nobody reads before clicking yes.
type Scope string

const (
	// ScopeRead permits observing the origin: opening it, reading it,
	// snapshotting it, screenshotting it.
	ScopeRead Scope = "read"
	// ScopeAct permits changing state on the origin: clicking, typing, filling,
	// submitting, uploading. It subsumes ScopeRead, because an agent that can
	// act on a page can read it.
	ScopeAct Scope = "act"
)

// Scopes lists the valid scopes, narrowest first.
func Scopes() []Scope { return []Scope{ScopeRead, ScopeAct} }

// ParseScope validates a scope written by a human or a config file.
func ParseScope(raw string) (Scope, error) {
	switch Scope(strings.ToLower(strings.TrimSpace(raw))) {
	case ScopeRead:
		return ScopeRead, nil
	case ScopeAct:
		return ScopeAct, nil
	default:
		return "", fmt.Errorf("scope must be read or act; got %q", raw)
	}
}

// covers reports whether a grant at scope s authorises a request needing want.
func (s Scope) covers(want Scope) bool {
	if s == want {
		return true
	}
	return s == ScopeAct && want == ScopeRead
}

// Decision is the answer a user gave. A recorded "deny" matters as much as a
// recorded "allow": without it an interactive prompt that was answered no is
// asked again on the very next action, which trains the user to click yes.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// Grant is one persisted consent record.
//
// MAC binds the authorising fields to the key in the profile's consent key file.
// Everything a forger would need to change to widen their access is inside it:
// the origin, the scope, the decision, the expiry, the grantor, and the category
// override. Fields outside the MAC (Note) are descriptive only and are never
// consulted by a decision.
type Grant struct {
	Origin    string    `json:"origin"`
	Scope     Scope     `json:"scope"`
	Decision  Decision  `json:"decision"`
	GrantedAt time.Time `json:"granted_at"`
	GrantedBy string    `json:"granted_by"`
	// Expiry is when the grant stops authorising. Zero means it never expires.
	Expiry time.Time `json:"expiry,omitzero"`
	// OverrideCategory names the shipped blocklist category this grant was
	// allowed to cross. Empty for an ordinary grant. It is inside the MAC, so a
	// forger cannot take an ordinary grant and relabel it as an override.
	OverrideCategory string `json:"override_category,omitempty"`
	Note             string `json:"note,omitempty"`
	MAC              string `json:"mac"`
}

// ErrBadMAC is returned for a record whose MAC does not verify against the
// profile's consent key.
var ErrBadMAC = errors.New("consent record is not authenticated by this profile's consent key")

// Expired reports whether the grant has passed its expiry at now.
func (g Grant) Expired(now time.Time) bool {
	return !g.Expiry.IsZero() && !now.Before(g.Expiry)
}

// macPayload is the exact byte string the MAC covers.
//
// Length-prefixing every field is not decoration. With plain separators, a
// grantor of "x\nallow" and an origin ending in the right place produce the same
// payload as a different record, and a forger who controls one field can shift
// the meaning of another. Each field is written as its decimal byte length, a
// colon, then its bytes, so no field's content can be read as part of another.
func (g Grant) macPayload() []byte {
	expiry := "never"
	if !g.Expiry.IsZero() {
		expiry = g.Expiry.UTC().Format(time.RFC3339Nano)
	}
	fields := []string{
		"brw-site-consent-v1",
		g.Origin,
		string(g.Scope),
		string(g.Decision),
		g.GrantedAt.UTC().Format(time.RFC3339Nano),
		expiry,
		g.GrantedBy,
		g.OverrideCategory,
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return []byte(b.String())
}

// Sign stamps the record's MAC with key. It replaces any MAC already present.
func (g *Grant) Sign(key []byte) {
	mac := hmac.New(sha256.New, key)
	mac.Write(g.macPayload())
	g.MAC = base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether the record's MAC matches its authorising fields under
// key. A record that fails is not a record: it is a line someone wrote into the
// file, and it is discarded rather than honoured.
func (g Grant) Verify(key []byte) error {
	want := hmac.New(sha256.New, key)
	want.Write(g.macPayload())
	got, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(g.MAC, "="))
	if err != nil {
		return ErrBadMAC
	}
	if !hmac.Equal(got, want.Sum(nil)) {
		return ErrBadMAC
	}
	return nil
}

// Valid reports whether the record is internally coherent, before any MAC check.
// A record with no origin or an unknown scope cannot authorise anything, so it
// is rejected even if its MAC verifies.
func (g Grant) Valid() error {
	if strings.TrimSpace(g.Origin) == "" {
		return errors.New("consent record has no origin")
	}
	if g.Scope != ScopeRead && g.Scope != ScopeAct {
		return fmt.Errorf("consent record for %s has scope %q, which is neither read nor act", g.Origin, g.Scope)
	}
	if g.Decision != DecisionAllow && g.Decision != DecisionDeny {
		return fmt.Errorf("consent record for %s has decision %q, which is neither allow nor deny", g.Origin, g.Decision)
	}
	if strings.TrimSpace(g.GrantedBy) == "" {
		return fmt.Errorf("consent record for %s names no grantor, so there is nobody to attribute the consent to", g.Origin)
	}
	return nil
}
