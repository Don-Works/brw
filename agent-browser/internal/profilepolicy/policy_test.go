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
			{"name":"revitt","user_data_dir":"~/Library/Application Support/Google/Chrome","profile_directory":"Profile 1","direct_cdp_allowed":false}
		],
		"transports": [
			{"name":"max-air","kind":"ssh-stdio","host":"max-air","user":"maxrevitt","app_dir":"~/Library/Application Support/agent-browser"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	policy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := policy.Find("revitt")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProfileDirectory != "Profile 1" {
		t.Fatalf("profile directory = %q", profile.ProfileDirectory)
	}
	if profile.DirectCDPAllowed {
		t.Fatal("revitt should not allow direct CDP in test policy")
	}
	if profile.UserDataDir == "" || profile.UserDataDir[0] == '~' {
		t.Fatalf("expected expanded user data dir, got %q", profile.UserDataDir)
	}
	transport, err := policy.FindTransport("max-air")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Host != "max-air" {
		t.Fatalf("transport host = %q", transport.Host)
	}
	if transport.AppDir != "~/Library/Application Support/agent-browser" {
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
