package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Chrome opt-in lane exists because Chrome asks a human to grant full CDP
// access to their signed-in profile. A brw that answered a missing endpoint by
// launching Chrome with a debugging flag would be granting itself that access,
// so the refusal lives in the function that would do the launching.
//
// The assertion is not only on the error: Launch creates the user data
// directory before it spawns anything, so a directory that still does not exist
// afterwards is what proves no launch was attempted.
func TestAttachOnlyRefusesToLaunchABrowser(t *testing.T) {
	userDataDir := filepath.Join(t.TempDir(), "would-be-profile")
	manager, err := New(context.Background(), Config{AttachOnly: true, UserDataDir: userDataDir})
	if err == nil {
		_ = manager.Close()
		t.Fatal("an attach-only config with no endpoint started a browser")
	}
	if !errors.Is(err, ErrAttachOnlyNoEndpoint) {
		t.Fatalf("error = %v, want ErrAttachOnlyNoEndpoint", err)
	}
	if _, statErr := os.Stat(userDataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the user data directory exists (%v), so a launch was attempted", statErr)
	}
}

// On a browser brw did not start, the user data directory is that browser's
// own — on the Chrome opt-in lane, the profile its user is signed into. brw
// reads it to find the endpoint and writes nothing to it, so it must create no
// directory inside it at all.
//
// The guard is in resolveDownloadDir rather than only in the caller that builds
// the config: this is the function that creates the directory, and a later edit
// that set UserDataDir on an attach lane for any other reason would otherwise
// silently start writing into that profile.
func TestAnAttachedBrowsersProfileIsNeverWrittenTo(t *testing.T) {
	profile := t.TempDir()
	// resolveDownloadDir returns a symlink-resolved path, and on macOS the temp
	// directory is reached through one, so compare against the resolved form or
	// both halves of this test pass for the wrong reason.
	resolvedProfile, err := filepath.EvalSymlinks(profile)
	if err != nil {
		t.Fatalf("resolve %s: %v", profile, err)
	}

	attached := &Manager{userDataDir: profile, attachedBrowser: true}
	if _, err := attached.resolveDownloadDir(); !errors.Is(err, ErrDownloadRoutingAttachedBrowser) {
		t.Fatalf("resolve download dir on an attached browser = %v, want ErrDownloadRoutingAttachedBrowser", err)
	}
	if entries, err := os.ReadDir(profile); err != nil || len(entries) != 0 {
		t.Fatalf("the browser's profile directory was written to: %v entries (err %v)", len(entries), err)
	}

	// The control: a browser brw started does stage inside its profile, so the
	// refusal above is about the browser and not about resolveDownloadDir
	// having stopped working.
	owned := &Manager{userDataDir: profile}
	ownedStaged, err := owned.resolveDownloadDir()
	if err != nil {
		t.Fatalf("resolve download dir for a browser brw started: %v", err)
	}
	if !strings.HasPrefix(ownedStaged, resolvedProfile) {
		t.Fatalf("a profile brw created staged outside itself: %s", ownedStaged)
	}
}

// brw_state seals cookies, and on a lane driving the browser the user is
// personally signed into those are their own. The refusal is a property of the
// lane, so it has to hold for every action and whether or not a snapshot store
// was ever configured — a refusal that only covered "save" would still let
// "list" and "delete" reach that browser's stored snapshots.
func TestSessionStateRefusedOnASignedInBrowser(t *testing.T) {
	for _, action := range []string{
		SessionStateActionSave,
		SessionStateActionRestore,
		SessionStateActionList,
		SessionStateActionDelete,
	} {
		t.Run(action, func(t *testing.T) {
			signedIn := &Manager{signedInProfile: true}
			_, err := signedIn.SessionState(context.Background(), SessionStateOptions{
				Action:     action,
				SnapshotID: "some-snapshot",
				Origins:    []string{"https://example.com"},
			})
			if !errors.Is(err, ErrSessionStateSignedIn) {
				t.Fatalf("%s on a signed-in browser = %v, want ErrSessionStateSignedIn", action, err)
			}

			// The control: the same call on a lane that is not the user's own
			// browser fails for the ordinary reason (no store configured), so
			// the refusal above came from the lane and not from the call shape.
			ordinary := &Manager{}
			_, err = ordinary.SessionState(context.Background(), SessionStateOptions{
				Action:     action,
				SnapshotID: "some-snapshot",
				Origins:    []string{"https://example.com"},
			})
			if errors.Is(err, ErrSessionStateSignedIn) {
				t.Fatalf("%s on an ordinary direct-CDP lane was refused as signed-in", action)
			}
			if !errors.Is(err, ErrSessionStateDisabled) {
				t.Fatalf("%s on an ordinary lane with no store = %v, want ErrSessionStateDisabled", action, err)
			}
		})
	}
}
