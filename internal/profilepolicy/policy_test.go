package profilepolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndFindProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`{
		"profiles": [
			{"name":"work-profile","user_data_dir":"~/Library/Application Support/Google/Chrome","profile_directory":"Profile 1","direct_cdp_allowed":false}
		],
		"transports": [
			{"name":"remote","kind":"ssh-stdio","host":"remote","user":"remote-user","app_dir":"~/Library/Application Support/brw"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	policy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := policy.Find("work-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProfileDirectory != "Profile 1" {
		t.Fatalf("profile directory = %q", profile.ProfileDirectory)
	}
	if profile.DirectCDPAllowed {
		t.Fatal("work-profile should not allow direct CDP in test policy")
	}
	if profile.UserDataDir == "" || profile.UserDataDir[0] == '~' {
		t.Fatalf("expected expanded user data dir, got %q", profile.UserDataDir)
	}
	transport, err := policy.FindTransport("remote")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Host != "remote" {
		t.Fatalf("transport host = %q", transport.Host)
	}
	if transport.AppDir != "~/Library/Application Support/brw" {
		t.Fatalf("transport app dir should preserve remote path, got %q", transport.AppDir)
	}
}

func TestFindRejectsUnknownProfile(t *testing.T) {
	policy := Policy{Profiles: []Profile{{Name: "allowed"}}}
	if _, err := policy.Find("not-allowed"); err == nil {
		t.Fatal("expected unknown profile to be rejected")
	}
}

func TestFindRejectsUnknownTransport(t *testing.T) {
	policy := Policy{Transports: []Transport{{Name: "allowed"}}}
	if _, err := policy.FindTransport("not-allowed"); err == nil {
		t.Fatal("expected unknown transport to be rejected")
	}
}

func TestResolveProfileUsesWorkspaceDefaultAndAllowedList(t *testing.T) {
	policy := Policy{
		WorkspaceBindings: []WorkspaceBinding{{
			Workspace:       "brw",
			DefaultProfile:  "work-profile",
			AllowedProfiles: []string{"work-profile", "local-profile"},
		}},
		Profiles: []Profile{
			{Name: "work-profile"},
			{Name: "local-profile"},
			{Name: "other-profile"},
		},
	}

	profile, err := policy.ResolveProfile("brw", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "work-profile" {
		t.Fatalf("default profile = %q", profile.Name)
	}

	profile, err = policy.ResolveProfile("brw", "local-profile")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "local-profile" {
		t.Fatalf("explicit profile = %q", profile.Name)
	}

	if _, err := policy.ResolveProfile("brw", "other-profile"); err == nil {
		t.Fatal("expected workspace profile allow-list to reject other-profile")
	}
}

func TestResolveTransportUsesWorkspaceDefaultAndAllowedList(t *testing.T) {
	policy := Policy{
		WorkspaceBindings: []WorkspaceBinding{{
			Workspace:         "brw",
			DefaultTransport:  "remote",
			AllowedTransports: []string{"remote", "local"},
		}},
		Transports: []Transport{
			{Name: "remote"},
			{Name: "local"},
			{Name: "other-host"},
		},
	}

	transport, err := policy.ResolveTransport("brw", "")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Name != "remote" {
		t.Fatalf("default transport = %q", transport.Name)
	}

	transport, err = policy.ResolveTransport("brw", "local")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Name != "local" {
		t.Fatalf("explicit transport = %q", transport.Name)
	}

	if _, err := policy.ResolveTransport("brw", "other-host"); err == nil {
		t.Fatal("expected workspace transport allow-list to reject other-host")
	}
}
