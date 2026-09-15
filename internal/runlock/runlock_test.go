package runlock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// TestKeyIsTheProfileNotTheDaemon is the property the whole package rests on.
// Two daemons can drive one browser profile — an --upstream-http proxy in front
// of a bridge daemon is the shipped example — so a key that moved with the
// daemon would give each of them a lock of its own while they fought over one
// tab.
func TestKeyIsTheProfileNotTheDaemon(t *testing.T) {
	profile := brwidentity.Identity{
		Workspace: "work", Profile: "chrome-work",
		UserDataDir: "/var/tmp/brw/chrome", ProfileDirectory: "Profile 1",
	}

	// Every field that describes the DAEMON rather than the profile, varied one
	// at a time. Each must leave the key alone.
	daemonOnly := map[string]brwidentity.Identity{
		"mode":                {Mode: "upstream-http"},
		"transport":           {Transport: brwidentity.TransportExtensionBridge},
		"headless":            {Headless: true},
		"ignore https errors": {IgnoreHTTPSErrors: true},
	}
	want := Key(profile)
	for name, overlay := range daemonOnly {
		candidate := profile
		if overlay.Mode != "" {
			candidate.Mode = overlay.Mode
		}
		if overlay.Transport != "" {
			candidate.Transport = overlay.Transport
		}
		candidate.Headless = overlay.Headless
		candidate.IgnoreHTTPSErrors = overlay.IgnoreHTTPSErrors
		if got := Key(candidate); got != want {
			t.Errorf("changing %s changed the lock key (%s vs %s), so two daemons on one profile would not serialise", name, got, want)
		}
	}

	// Every field that DOES name the profile has to change the key, or two
	// different browsers would take turns for no reason.
	profileFields := map[string]brwidentity.Identity{
		"workspace":         {Workspace: "other", Profile: "chrome-work", UserDataDir: "/var/tmp/brw/chrome", ProfileDirectory: "Profile 1"},
		"profile":           {Workspace: "work", Profile: "chrome-personal", UserDataDir: "/var/tmp/brw/chrome", ProfileDirectory: "Profile 1"},
		"user data dir":     {Workspace: "work", Profile: "chrome-work", UserDataDir: "/var/tmp/brw/chromium", ProfileDirectory: "Profile 1"},
		"profile directory": {Workspace: "work", Profile: "chrome-work", UserDataDir: "/var/tmp/brw/chrome", ProfileDirectory: "Profile 2"},
	}
	for name, other := range profileFields {
		if Key(other) == want {
			t.Errorf("a different %s produced the same lock key, so two profiles would serialise against each other", name)
		}
	}

	// Two spellings of one directory are one profile.
	trailing := profile
	trailing.UserDataDir = "/var/tmp/brw/chrome/"
	if Key(trailing) != want {
		t.Error("a trailing separator on user_data_dir produced a different key")
	}

	// A daemon that says nothing about its profile still gets a key, and every
	// such daemon gets the SAME key: over-serialising is the safe direction.
	if got := Key(brwidentity.Identity{}); got != Unidentified {
		t.Errorf("Key(empty) = %q, want %q", got, Unidentified)
	}
	if Key(brwidentity.Identity{Mode: "direct"}) != Unidentified {
		t.Error("a daemon that reports only its mode must still fall back to the shared key")
	}
}

// TestSecondRunWaitsForTheFirst drives the actual contention: two holders, one
// lock, no overlap.
func TestSecondRunWaitsForTheFirst(t *testing.T) {
	dir := t.TempDir()
	var concurrent, peak int64

	hold := func() error {
		lock, err := Acquire(context.Background(), dir, "profile-key", 5*time.Second)
		if err != nil {
			return err
		}
		defer lock.Release()
		now := atomic.AddInt64(&concurrent, 1)
		for {
			high := atomic.LoadInt64(&peak)
			if now <= high || atomic.CompareAndSwapInt64(&peak, high, now) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		atomic.AddInt64(&concurrent, -1)
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errs[index] = hold()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("holder %d: %v", i, err)
		}
	}
	if peak != 1 {
		t.Fatalf("%d runs held the profile at once; the lock did not serialise them", peak)
	}
}

// TestZeroWaitRefusesInsteadOfQueueing gives a scheduler the other half of the
// contract: a run that must not pile up behind its predecessor can say so and
// get a named refusal rather than a timeout it has to interpret.
func TestZeroWaitRefusesInsteadOfQueueing(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(context.Background(), dir, "profile-key", 0)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(context.Background(), dir, "profile-key", 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Acquire = %v, want ErrBusy", err)
	}

	// A different profile is not contended by this one.
	other, err := Acquire(context.Background(), dir, "another-profile-key", 0)
	if err != nil {
		t.Fatalf("a different profile was refused: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// And once the holder lets go, the next run gets it.
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := Acquire(context.Background(), dir, "profile-key", 0)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Releasing twice is not an error: the caller's defer and an explicit
	// release on the success path must not fight.
	if err := third.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

// TestBoundedWaitGivesUpWithABusyError keeps a queueing run from blocking
// forever: a scheduler that fires hourly needs the run that cannot start to end,
// not to accumulate.
func TestBoundedWaitGivesUpWithABusyError(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "profile-key", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	started := time.Now()
	if _, err := Acquire(context.Background(), dir, "profile-key", 300*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("Acquire = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("gave up after %s, which is less than the wait it was given", elapsed)
	}
}

// TestCancelledWaitStopsWaiting: a scheduler that kills the job must not leave a
// process spinning on a lock it will never get.
func TestCancelledWaitStopsWaiting(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "profile-key", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := Acquire(ctx, dir, "profile-key", time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
}
