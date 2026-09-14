package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/siteconsent"
)

func consentFixturePolicy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "browser-profiles.json")
	if err := os.WriteFile(path, []byte(`{"profiles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildSiteConsentOff(t *testing.T) {
	guard, err := buildSiteConsent(siteConsentOptions{policyPath: consentFixturePolicy(t)})
	if err != nil {
		t.Fatalf("consent off must not error: %v", err)
	}
	if guard.Enabled() {
		t.Fatal("consent is opt-in; a daemon without the flag must gate nothing")
	}
}

// TestBuildSiteConsentRefusesHalfConfiguredGates proves a flag that could only
// do nothing is refused rather than accepted.
func TestBuildSiteConsentRefusesHalfConfiguredGates(t *testing.T) {
	policy := consentFixturePolicy(t)
	cases := []struct {
		name string
		opts siteConsentOptions
		want string
	}{
		{
			name: "confirm-actions without site-consent",
			opts: siteConsentOptions{policyPath: policy, confirm: true},
			want: "--confirm-actions needs --site-consent",
		},
		{
			name: "prompt without site-consent",
			opts: siteConsentOptions{policyPath: policy, prompt: true},
			want: "--site-consent-prompt needs --site-consent",
		},
		{
			name: "prompt with mcp",
			opts: siteConsentOptions{policyPath: policy, enabled: true, prompt: true, mcpMode: true},
			want: "cannot be used with --mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildSiteConsent(c.opts)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestBuildSiteConsentCreatesTheStoreBesideThePolicy(t *testing.T) {
	policy := consentFixturePolicy(t)
	guard, err := buildSiteConsent(siteConsentOptions{policyPath: policy, enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !guard.Enabled() {
		t.Fatal("the guard reports itself disabled")
	}
	dir := filepath.Dir(policy)
	if guard.Store().Path() != siteconsent.StorePath(dir) {
		t.Fatalf("store at %q, want it beside the policy at %q", guard.Store().Path(), siteconsent.StorePath(dir))
	}
	info, err := os.Stat(siteconsent.KeyPath(dir))
	if err != nil {
		t.Fatalf("the consent key was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("consent key is mode %#o; it must be 0600 so no other account can mint records", perm)
	}
	if guard.Interactive() {
		t.Fatal("a daemon with no --site-consent-prompt must be non-interactive")
	}
}

// TestAdminConfigCannotBeWeakenedByAFlag proves the direction of the OR: the
// flag can only add the gate, never remove one the admin config set.
func TestAdminConfigCannotBeWeakenedByAFlag(t *testing.T) {
	policy := consentFixturePolicy(t)
	configPath := filepath.Join(filepath.Dir(policy), "site-consent.json")
	if err := os.WriteFile(configPath, []byte(`{"confirm_actions":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := buildSiteConsent(siteConsentOptions{policyPath: policy, enabled: true, confirm: false})
	if err != nil {
		t.Fatal(err)
	}
	if !guard.ConfirmActions() {
		t.Fatal("the admin config's confirm_actions was dropped when the flag was absent")
	}
}

func TestAdminConfigIsDiscoveredBesideThePolicy(t *testing.T) {
	policy := consentFixturePolicy(t)
	configPath := filepath.Join(filepath.Dir(policy), "site-consent.json")
	if err := os.WriteFile(configPath, []byte(`{"allowed_origins":["intranet.example.test"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := buildSiteConsent(siteConsentOptions{policyPath: policy, enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Authorize("https://intranet.example.test/app", siteconsent.ScopeAct); err != nil {
		t.Fatalf("the admin allowlist was not loaded: %v", err)
	}
	if err := guard.Authorize("https://elsewhere.test/app", siteconsent.ScopeAct); err == nil {
		t.Fatal("an origin outside the admin allowlist was authorised")
	}
}

func TestAdminConfigTypoIsRefused(t *testing.T) {
	policy := consentFixturePolicy(t)
	configPath := filepath.Join(filepath.Dir(policy), "site-consent.json")
	// blocked_origin, not blocked_origins: silently ignoring it would leave an
	// operator believing a site was blocked when it was not.
	if err := os.WriteFile(configPath, []byte(`{"blocked_origin":["evil.test"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSiteConsent(siteConsentOptions{policyPath: policy, enabled: true}); err == nil {
		t.Fatal("a misspelled admin config key was accepted")
	}
}
