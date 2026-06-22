package brwidentity

import "testing"

func TestIdentityMismatchesComparesOnlyExpectedFields(t *testing.T) {
	got := Identity{
		Workspace:        "client-a",
		Profile:          "chrome-a",
		UserDataDir:      "/profiles/a",
		ProfileDirectory: "Profile 1",
		Mode:             "bridge",
	}
	if mismatches := got.Mismatches(Identity{Workspace: "client-a", Profile: "chrome-a"}); len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
	if mismatches := got.Mismatches(Identity{Workspace: "client-b"}); len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want one workspace mismatch", mismatches)
	}
}

func TestIdentityEmpty(t *testing.T) {
	empty := Identity{}
	if !empty.Empty() {
		t.Fatal("zero identity should be empty")
	}
	nonEmpty := Identity{Workspace: "client-a"}
	if nonEmpty.Empty() {
		t.Fatal("workspace identity should not be empty")
	}
}
