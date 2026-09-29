package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/profileroster"
)

func TestProfilesCreateListAndPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	policy := filepath.Join(home, "browser-profiles.json")

	var out bytes.Buffer
	if err := runProfiles([]string{"create", "bookkeeper", "--account", "agent@example.test", "--profile-policy", policy}, &out); err != nil {
		t.Fatal(err)
	}
	var created profileroster.CreateResult
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatalf("create printed %q: %v", out.String(), err)
	}
	if !created.Created || created.Profile.UserDataDir != "~/.brw/profiles/bookkeeper" || !strings.Contains(created.ServiceCommand, "brwctl setup") {
		t.Fatalf("created = %+v", created)
	}

	if err := runProfiles([]string{"pin", "bookkeeper", "--origin", "https://go.example.test", "--profile-policy", policy}, &out); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runProfiles([]string{"sessions", "bookkeeper", "--profile-policy", policy}, &out); err != nil {
		t.Fatal(err)
	}
	var well profileroster.Well
	if err := json.Unmarshal(out.Bytes(), &well); err != nil {
		t.Fatal(err)
	}
	if well.AcceptsDrop || well.Account != "agent@example.test" || len(well.Chips) != 2 {
		t.Fatalf("well = %+v", well)
	}
}

func TestProfilesRejectsBadUsage(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "absent.json")
	for _, args := range [][]string{
		nil,
		{"frobnicate"},
		{"create"},
		{"copy", "--from", "a", "--profile-policy", policy},
		{"pin", "a", "--profile-policy", policy},
	} {
		if err := runProfiles(args, &bytes.Buffer{}); err == nil {
			t.Errorf("runProfiles(%q) succeeded", args)
		}
	}
}

func TestInterleavedMovesFlagsAheadOfPositionals(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.String("account", "", "")
	fs.Bool("dry", false, "")
	got := interleaved([]string{"name", "--account", "a@b", "--dry", "--x=1", "--", "--literal"}, fs)
	want := []string{"--account", "a@b", "--dry", "--x=1", "name", "--literal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interleaved = %q, want %q", got, want)
	}
}
