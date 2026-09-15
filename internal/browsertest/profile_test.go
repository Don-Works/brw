package browsertest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The contract this package exists for, asserted without a browser: the
// shutdown runs first, in reverse registration order, and the directory is gone
// once the test that owned it is over — including anything the shutdown itself
// wrote on its way out, which is the write a real Chrome helper is still doing
// when testing's own cleanup reaches for the directory.
func TestProfileRunsEveryStopBeforeItReclaimsTheDirectory(t *testing.T) {
	var (
		order []string
		dir   string
	)
	t.Run("owner", func(t *testing.T) {
		profile := NewProfile(t)
		dir = profile.Dir()
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("the profile directory was not created: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Default"), []byte("profile state"), 0o600); err != nil {
			t.Fatal(err)
		}
		profile.StopWith(func() { order = append(order, "outer") })
		profile.StopWith(func() {
			order = append(order, "inner")
			if err := os.WriteFile(filepath.Join(dir, "late-write"), []byte("still flushing"), 0o600); err != nil {
				t.Errorf("late write: %v", err)
			}
		})
	})

	if want := []string{"inner", "outer"}; !slices.Equal(order, want) {
		t.Errorf("stops ran %v, want %v — the most recent registration has to unwind first", order, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the profile outlived the test that owned it: stat = %v", err)
	}
}

// A profile nothing was started against still reclaims, so a test that skips
// before its browser exists reports the skip rather than a cleanup failure.
func TestProfileWithNoStopStillReclaims(t *testing.T) {
	var dir string
	t.Run("owner", func(t *testing.T) {
		dir = NewProfile(t).Dir()
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the profile outlived the test that owned it: stat = %v", err)
	}
}
