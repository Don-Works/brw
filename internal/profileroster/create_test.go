package profileroster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

func TestCreateGivesEachProfileItsOwnDirectoryAndPort(t *testing.T) {
	home := t.TempDir()
	policyPath := filepath.Join(home, "browser-profiles.json")
	first, err := Create(CreateRequest{
		Name:       "Bookkeeper",
		Account:    "agent.bookkeeper@example.test",
		PolicyPath: policyPath,
		Home:       home,
		GOOS:       "linux",
		BRWDPath:   "/opt/brw/brwd",
		Now:        time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Profile.Name != "bookkeeper" {
		t.Fatalf("first = %+v", first)
	}
	if first.Profile.UserDataDir != "~/.brw/profiles/bookkeeper" {
		t.Fatalf("udd = %q", first.Profile.UserDataDir)
	}
	if info, err := os.Stat(filepath.Join(home, ".brw", "profiles", "bookkeeper")); err != nil || !info.IsDir() {
		t.Fatalf("profile directory not created: %v", err)
	}
	if !first.Profile.DirectCDPAllowed || first.Profile.ExtensionBridgeAllowed {
		t.Fatalf("lanes wrong: %+v", first.Profile)
	}
	if first.HTTPAddr != "127.0.0.1:17312" {
		t.Fatalf("http addr = %q", first.HTTPAddr)
	}
	if len(first.Profile.Pins) != 1 || first.Profile.Pins[0].Account != "agent.bookkeeper@example.test" {
		t.Fatalf("pins = %+v", first.Profile.Pins)
	}
	if !strings.Contains(first.RunCommand, "--profile bookkeeper") || !strings.Contains(first.RunCommand, "--http 127.0.0.1:17312") {
		t.Fatalf("run command = %q", first.RunCommand)
	}
	if !strings.Contains(first.ServiceCommand, "brwctl setup --transport direct-cdp --profile bookkeeper --workspace brw-bookkeeper") {
		t.Fatalf("service command = %q", first.ServiceCommand)
	}

	second, err := Create(CreateRequest{Name: "payroll", PolicyPath: policyPath, Home: home, GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Profile.UserDataDir == first.Profile.UserDataDir || second.HTTPAddr == first.HTTPAddr {
		t.Fatalf("two profiles share a directory or port: %+v %+v", first.Profile, second.Profile)
	}

	again, err := Create(CreateRequest{Name: "bookkeeper", PolicyPath: policyPath, Home: home, GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.HTTPAddr != first.HTTPAddr {
		t.Fatalf("re-create changed something: %+v", again)
	}

	written, _, err := setup.LoadPolicyFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Profiles) != 2 {
		t.Fatalf("policy profiles = %+v", written.Profiles)
	}
	if _, ok := written.FindWorkspace("brw-bookkeeper"); !ok {
		t.Fatal("no workspace binding for the new profile")
	}
}

func TestCreateLeavesAnExistingProfileAlone(t *testing.T) {
	home := t.TempDir()
	policyPath := filepath.Join(home, "browser-profiles.json")
	existing := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{
		Name: "work", UserDataDir: "~/Library/Application Support/Google/Chrome", ExtensionBridgeAllowed: true,
		BridgeHTTPAddr: "127.0.0.1:17410", BridgeWSAddr: "127.0.0.1:17411",
	}}}
	if _, err := setup.WritePolicy(policyPath, existing, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := Create(CreateRequest{Name: "work", PolicyPath: policyPath, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if got.Created || got.Profile.DirectCDPAllowed {
		t.Fatalf("create rewrote an existing profile: %+v", got)
	}
	fresh, err := Create(CreateRequest{Name: "fresh", PolicyPath: policyPath, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HTTPAddr != "127.0.0.1:17412" {
		t.Fatalf("new port %q collides with the existing bridge pair", fresh.HTTPAddr)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Bookkeeper": "bookkeeper", "book keeper": "book-keeper", "a_b": "a-b"} {
		if got, err := Slug(in); err != nil || got != want {
			t.Errorf("Slug(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "2bad", "../x", "a/b", strings.Repeat("a", 70)} {
		if _, err := Slug(bad); err == nil {
			t.Errorf("Slug(%q) accepted", bad)
		}
	}
	if Namespace("book-keeper") != "brw_book_keeper" || Workspace("bookkeeper") != "brw-bookkeeper" {
		t.Fatal("namespace or workspace naming changed")
	}
}

func TestChipsFromCookies(t *testing.T) {
	now := time.Unix(2000, 0)
	chips := chipsFromCookies([]browser.Cookie{
		{Name: "SID", Value: "never-shown", Domain: ".google.com", Expires: 3000},
		{Name: "old", Domain: "stale.test", Expires: 1000},
		{Name: "s", Domain: "www.session.test", Session: true},
	}, []profilepolicy.Pin{{Origin: "https://go.xero.com", Account: "agent@example.test"}}, "agent@example.test", now)
	got := map[string]Chip{}
	for _, c := range chips {
		got[c.Domain] = c
	}
	if got["google.com"].Health != HealthSignedIn || got["google.com"].Account != "agent@example.test" {
		t.Fatalf("google chip = %+v", got["google.com"])
	}
	if got["stale.test"].Health != HealthExpired || got["stale.test"].Account != "" {
		t.Fatalf("stale chip = %+v", got["stale.test"])
	}
	if got["session.test"].Health != HealthSignedIn {
		t.Fatalf("session chip = %+v", got["session.test"])
	}
	if c := got["go.xero.com"]; c.Health != HealthMissing || !c.Pinned || c.Account != "agent@example.test" {
		t.Fatalf("pinned chip = %+v", c)
	}
}

func TestGoogleAccountNeverReadsADailyProfile(t *testing.T) {
	home := isolatedHome(t)
	daily := dailyChromeDir(t, home)
	owned := filepath.Join(home, ".brw", "profiles", "agent")
	prefs := []byte(`{"account_info":[{"email":"someone@example.test"}]}`)
	for _, dir := range []string{daily, owned} {
		if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Default", "Preferences"), prefs, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := GoogleAccount(profilepolicy.Profile{UserDataDir: daily}); got != "" {
		t.Fatalf("read %q from a daily profile", got)
	}
	if got := GoogleAccount(profilepolicy.Profile{UserDataDir: owned}); got != "someone@example.test" {
		t.Fatalf("GoogleAccount = %q", got)
	}
}

func TestAddPinValidatesAndDeduplicates(t *testing.T) {
	home := t.TempDir()
	policyPath := filepath.Join(home, "browser-profiles.json")
	if _, err := Create(CreateRequest{Name: "agent", PolicyPath: policyPath, Home: home}); err != nil {
		t.Fatal(err)
	}
	if err := AddPin(policyPath, "agent", "javascript:alert(1)", "", ""); err == nil {
		t.Fatal("a non-http origin was pinned")
	}
	if err := AddPin(policyPath, "missing", "https://example.test", "", ""); err == nil {
		t.Fatal("a pin on an unknown profile was accepted")
	}
	for i := 0; i < 2; i++ {
		if err := AddPin(policyPath, "agent", "https://app.example.test/login?x=1", "a@example.test", ""); err != nil {
			t.Fatal(err)
		}
	}
	policy, _, _ := setup.LoadPolicyFile(policyPath)
	p, _ := policy.Find("agent")
	if len(p.Pins) != 1 || p.Pins[0].Origin != "https://app.example.test" || p.Pins[0].Label != "app.example.test" {
		t.Fatalf("pins = %+v", p.Pins)
	}
}
