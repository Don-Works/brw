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
type Scope string

const (
	// ScopeRead permits observing the origin: opening it, reading it, snapshotting it, screenshotting it.
	ScopeRead Scope = "read"
	// ScopeAct permits changing state on the origin: clicking, typing, filling, submitting, uploading.
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

func (s Scope) covers(want Scope) bool {
	if s == want {
		return true
	}
	return s == ScopeAct && want == ScopeRead
}

// Decision is the answer a user gave.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// Grant is one persisted consent record.
type Grant struct {
	Origin    string    `json:"origin"`
	Scope     Scope     `json:"scope"`
	Decision  Decision  `json:"decision"`
	GrantedAt time.Time `json:"granted_at"`
	GrantedBy string    `json:"granted_by"`
	// Expiry is when the grant stops authorising.
	Expiry time.Time `json:"expiry,omitzero"`
	// OverrideCategory names the shipped blocklist category this grant was allowed to cross.
	OverrideCategory string `json:"override_category,omitempty"`
	Note             string `json:"note,omitempty"`
	MAC              string `json:"mac"`
}

// ErrBadMAC is returned for a record whose MAC does not verify against the profile's consent key.
var ErrBadMAC = errors.New("consent record is not authenticated by this profile's consent key")

// Expired reports whether the grant has passed its expiry at now.
func (g Grant) Expired(now time.Time) bool {
	return !g.Expiry.IsZero() && !now.Before(g.Expiry)
}

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

// Sign stamps the record's MAC with key.
func (g *Grant) Sign(key []byte) {
	mac := hmac.New(sha256.New, key)
	mac.Write(g.macPayload())
	g.MAC = base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether the record's MAC matches its authorising fields under key.
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
