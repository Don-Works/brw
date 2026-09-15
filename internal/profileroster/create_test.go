package profileroster

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

func TestCreateUsesUniqueUserDataDir(t *testing.T) {
	home := t.TempDir()
	policyPath := filepath.Join(home, "browser-profiles.json")
	first, err := Create(CreateRequest{
		Name:           "bookkeeper",
		Account:        "agent.bookkeeper@xyz.com",
		Browser:        "chromium",
		PolicyPath:     policyPath,
		Home:           home,
		GOOS:           "linux",
		InstallService: false,
		Now:            time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatal("first create should be new")
	}
	if first.Profile.UserDataDir != "~/.brw/profiles/bookkeeper" {
		t.Fatalf("udd = %q", first.Profile.UserDataDir)
	}
	if !first.Profile.DirectCDPAllowed || first.Profile.ExtensionBridgeAllowed {
		t.Fatalf("lanes wrong: %+v", first.Profile)
	}
	if first.Profile.BridgeHTTPAddr == "" {
		t.Fatal("direct-CDP profile needs a control HTTP addr for discovery")
	}
	if len(first.Profile.Pins) != 1 || first.Profile.Pins[0].Account != "agent.bookkeeper@xyz.com" {
		t.Fatalf("pins = %+v", first.Profile.Pins)
	}

	second, err := Create(CreateRequest{
		Name:           "whatever",
		Browser:        "chromium",
		PolicyPath:     policyPath,
		Home:           home,
		GOOS:           "linux",
		InstallService: false,
		Now:            time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Profile.UserDataDir == first.Profile.UserDataDir {
		t.Fatal("two profiles must not share a user-data-dir")
	}
	if second.Profile.BridgeHTTPAddr == first.Profile.BridgeHTTPAddr {
		t.Fatal("two profiles must not share a control port")
	}

	again, err := Create(CreateRequest{
		Name:           "bookkeeper",
		PolicyPath:     policyPath,
		Home:           home,
		GOOS:           "linux",
		InstallService: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created {
		t.Fatal("re-create of the same slug must be a no-op")
	}

	loaded, err := profilepolicy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Profiles) != 2 {
		t.Fatalf("profiles = %d", len(loaded.Profiles))
	}
}

func TestChipsFromCookies(t *testing.T) {
	chips := chipsFromCookies(nil, []profilepolicy.Pin{{
		Origin:  "https://go.xero.com",
		Account: "agent.bookkeeper@xyz.com",
	}}, "")
	if len(chips) != 1 || chips[0].Domain != "go.xero.com" || chips[0].Health != HealthMissing {
		t.Fatalf("chips = %+v", chips)
	}
}

func TestRegistrableDomainAndDailyChrome(t *testing.T) {
	if !isDailyChromeDir("~/Library/Application Support/Google/Chrome") {
		t.Fatal("expected daily chrome detection")
	}
	if RegistrableDomain(".www.Google.com") != "google.com" {
		t.Fatalf("%q", RegistrableDomain(".www.Google.com"))
	}
}
