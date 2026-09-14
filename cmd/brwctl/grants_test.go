package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// consentFixture points the grants subcommands at a throwaway config directory
// by handing them a profile policy path inside it.
func consentFixture(t *testing.T) (dir, policy string) {
	t.Helper()
	dir = t.TempDir()
	policy = filepath.Join(dir, "browser-profiles.json")
	if err := os.WriteFile(policy, []byte(`{"profiles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, policy
}

func runGrants(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	var err error
	switch args[0] {
	case "list":
		err = grantsList(args[1:], &out)
	case "allow":
		err = grantsAllow(args[1:], &out)
	case "revoke":
		err = grantsRevoke(args[1:], &out)
	case "revoke-all":
		err = grantsRevokeAll(args[1:], &out)
	case "ledger":
		err = grantsLedger(args[1:], &out)
	default:
		t.Fatalf("unknown grants subcommand %q", args[0])
	}
	return out.String(), err
}

func TestGrantsAllowListRevokeRoundTrip(t *testing.T) {
	_, policy := consentFixture(t)

	if out, err := runGrants(t, "list", "--profile-policy", policy); err != nil || !strings.Contains(out, "no site permission grants") {
		t.Fatalf("empty list: out=%q err=%v", out, err)
	}
	if _, err := runGrants(t, "allow", "--profile-policy", policy, "--scope", "act", "https://example.test"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	out, err := runGrants(t, "list", "--profile-policy", policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "https://example.test") || !strings.Contains(out, "act") {
		t.Fatalf("list did not show the grant: %q", out)
	}
	if out, err := runGrants(t, "revoke", "--profile-policy", policy, "https://example.test"); err != nil || !strings.Contains(out, "revoked 1") {
		t.Fatalf("revoke: out=%q err=%v", out, err)
	}
	if out, err := runGrants(t, "list", "--profile-policy", policy); err != nil || !strings.Contains(out, "no site permission grants") {
		t.Fatalf("list after revoke: out=%q err=%v", out, err)
	}
}

func TestGrantsRevokeAllClearsEverything(t *testing.T) {
	_, policy := consentFixture(t)
	for _, origin := range []string{"https://one.test", "https://two.test"} {
		if _, err := runGrants(t, "allow", "--profile-policy", policy, origin); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runGrants(t, "revoke-all", "--profile-policy", policy)
	if err != nil || !strings.Contains(out, "revoked 2") {
		t.Fatalf("revoke-all: out=%q err=%v", out, err)
	}
}

// TestGrantsAllowRefusesABlocklistedCategoryWithoutTheFlag is acceptance
// criterion 2 at the operator surface, ledger check included.
func TestGrantsAllowRefusesABlocklistedCategoryWithoutTheFlag(t *testing.T) {
	_, policy := consentFixture(t)
	_, err := runGrants(t, "allow", "--profile-policy", policy, "https://paypal.com")
	if err == nil {
		t.Fatal("a blocklisted origin was granted with no override")
	}
	if !strings.Contains(err.Error(), "financial-services") || !strings.Contains(err.Error(), "override") {
		t.Fatalf("the refusal must name the category and the override: %v", err)
	}
	if out, _ := runGrants(t, "list", "--profile-policy", policy); strings.Contains(out, "paypal.com") {
		t.Fatalf("a refused grant was still written: %q", out)
	}

	out, err := runGrants(t, "allow", "--profile-policy", policy,
		"--override-category", "financial-services", "--note", "fixture reason", "https://paypal.com")
	if err != nil {
		t.Fatalf("override grant: %v", err)
	}
	if !strings.Contains(out, "financial-services") {
		t.Fatalf("the override was not reported: %q", out)
	}

	ledger, err := runGrants(t, "ledger", "--profile-policy", policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ledger, "category-override") || !strings.Contains(ledger, "https://paypal.com") {
		t.Fatalf("the override is not in the ledger: %q", ledger)
	}
	if !strings.Contains(ledger, currentActor()) {
		t.Fatalf("the ledger does not record who made the override: %q", ledger)
	}
}

// TestGrantsListReportsForgedRecords proves the operator surface shows a
// hand-written record as refused rather than silently dropping it.
func TestGrantsListReportsForgedRecords(t *testing.T) {
	dir, policy := consentFixture(t)
	if _, err := runGrants(t, "allow", "--profile-policy", policy, "https://real.test"); err != nil {
		t.Fatal(err)
	}
	path := siteconsent.StorePath(dir)
	forged := `{"version":1,"grants":[{"origin":"https://attacker.test","scope":"act","decision":"allow","granted_at":"2026-03-01T12:00:00Z","granted_by":"fixture-user","mac":"bm90LWEtcmVhbC1tYWM"}]}`
	if err := os.WriteFile(path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runGrants(t, "list", "--profile-policy", policy, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Grants   []map[string]any `json:"grants"`
		Rejected []struct {
			Origin string `json:"origin"`
			Reason string `json:"reason"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(parsed.Grants) != 0 {
		t.Fatalf("a forged record was listed as a grant: %+v", parsed.Grants)
	}
	if len(parsed.Rejected) != 1 || !strings.Contains(parsed.Rejected[0].Reason, "not authenticated") {
		t.Fatalf("the forgery was not surfaced as a refusal: %+v", parsed.Rejected)
	}
}

func TestGrantsRejectsBadArguments(t *testing.T) {
	_, policy := consentFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "allow with no origin", args: []string{"allow", "--profile-policy", policy}},
		{name: "allow with two origins", args: []string{"allow", "--profile-policy", policy, "https://a.test", "https://b.test"}},
		{name: "allow with a bad scope", args: []string{"allow", "--profile-policy", policy, "--scope", "sideways", "https://a.test"}},
		{name: "revoke with no origin", args: []string{"revoke", "--profile-policy", policy}},
		{name: "list with an argument", args: []string{"list", "--profile-policy", policy, "extra"}},
		{name: "revoke-all with an argument", args: []string{"revoke-all", "--profile-policy", policy, "extra"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := runGrants(t, c.args...); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestGrantsCommandRejectsAnUnknownSubcommand(t *testing.T) {
	if err := grantsCommand([]string{"sideways"}); err == nil {
		t.Fatal("expected an error for an unknown grants subcommand")
	}
	if err := grantsCommand(nil); err == nil {
		t.Fatal("expected a usage error for no grants subcommand")
	}
}
