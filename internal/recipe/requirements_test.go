package recipe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// profileSurface is a fakeSurface that answers the profile-session question
// with whatever a test wants, so both halves of the gate are exercised.
type profileSurface struct {
	*fakeSurface
	answer error
}

func (p profileSurface) CheckProfileSession() error { return p.answer }

func signedInRecipe() Recipe {
	value := runRecipe()
	value.Requires = []string{RequiresProfileSession}
	return value
}

// Acceptance 3: a recipe that declares it needs the signed-in profile refuses
// on a cloud target, with a named reason, and does not silently run
// unauthenticated. "Does not run" is the half that matters, so it is asserted
// on the surface's own counters rather than on the error alone.
func TestARecipeNeedingTheSignedInProfileRefusesOnAProviderBackedBrowser(t *testing.T) {
	surface := newFakeSurface()
	refusal := browser.RemoteUnavailableError("profile_session")
	runner := Runner{Surface: profileSurface{fakeSurface: surface, answer: refusal}}

	_, err := runner.Run(context.Background(), signedInRecipe(), map[string]string{"month": "2026-09"})
	if err == nil {
		t.Fatal("a recipe requiring the signed-in profile ran on a provider-backed browser")
	}
	if !errors.Is(err, browser.ErrRemoteTargetUnsupported) {
		t.Fatalf("refusal = %v, want the remote-capability class so a caller can branch on it", err)
	}
	if !strings.Contains(err.Error(), RequiresProfileSession) {
		t.Fatalf("refusal = %v, want it to name the requirement", err)
	}
	if !strings.Contains(err.Error(), browser.RemoteUnavailable["profile_session"]) {
		t.Fatalf("refusal = %v, want the declared reason", err)
	}
	// Nothing ran. A refusal after step one would have left a half-filled form
	// on an anonymous session, which is the outcome the declaration exists to
	// prevent.
	if surface.fills != 0 || surface.clicks != 0 || surface.navigations != 0 {
		t.Fatalf("the recipe touched the browser before refusing: %+v", surface)
	}
}

// The same recipe has to RUN where the profile is available, or the guard is a
// blanket refusal rather than a gate.
func TestARecipeNeedingTheSignedInProfileRunsWhereTheProfileExists(t *testing.T) {
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error { f.emit("text.present", "ready"); return nil }
	runner := Runner{Surface: profileSurface{fakeSurface: surface, answer: nil}}
	result, err := runner.Run(context.Background(), signedInRecipe(), map[string]string{"month": "2026-09"})
	if err != nil {
		t.Fatalf("a recipe requiring the signed-in profile was refused on a signed-in browser: %v", err)
	}
	if result.Status != "done" || surface.clicks != 1 || surface.fills != 1 {
		t.Fatalf("result=%+v surface=%+v", result, surface)
	}
}

// A surface that cannot say is not "probably the signed-in browser". Failing
// open here would make the declaration meaningless on every transport that has
// not implemented the capability.
func TestARequirementFailsClosedOnASurfaceThatCannotAnswer(t *testing.T) {
	surface := newFakeSurface()
	_, err := (Runner{Surface: surface}).Run(context.Background(), signedInRecipe(), map[string]string{"month": "2026-09"})
	if err == nil {
		t.Fatal("a surface with no opinion ran a recipe that requires the signed-in profile")
	}
	if !strings.Contains(err.Error(), "cannot say") {
		t.Fatalf("refusal = %v, want it to say the surface could not answer", err)
	}
	if surface.fills != 0 || surface.clicks != 0 {
		t.Fatalf("the recipe touched the browser before refusing: %+v", surface)
	}
}

// Requirements is a closed domain, and both halves have to agree on it: the
// schema accepts exactly these names, and the runner honours exactly these
// names. A name in one and not the other is a declaration with nothing behind
// it, which is worse than no declaration at all.
func TestEveryDeclaredRequirementIsValidatedAndHonoured(t *testing.T) {
	for _, name := range Requirements {
		t.Run(name, func(t *testing.T) {
			value := runRecipe()
			value.Requires = []string{name}
			if err := Validate(value); err != nil {
				t.Fatalf("the schema refuses the declared requirement %q: %v", name, err)
			}
			// Honoured means the runner has a branch for it. A name the switch
			// does not recognise falls through to the refusal naming it as
			// unimplemented, which is what this looks for the absence of.
			err := (Runner{Surface: profileSurface{fakeSurface: newFakeSurface(), answer: nil}}).checkRequirements(value)
			if err != nil && strings.Contains(err.Error(), "does not implement") {
				t.Fatalf("requirement %q is accepted by the schema and not implemented by the runner: %v", name, err)
			}
		})
	}
	for _, name := range []string{"", "signed_in", "profile-session", "PROFILE_SESSION", "profile_session ", "installed_profile"} {
		value := runRecipe()
		value.Requires = []string{name}
		if err := Validate(value); err == nil {
			t.Errorf("the schema accepted requirement %q, which is not in %v", name, Requirements)
		}
		// And the runner refuses it too, so a recipe that somehow reached it
		// without validation does not run with the requirement ignored.
		if err := (Runner{Surface: newFakeSurface()}).checkRequirements(value); err == nil {
			t.Errorf("the runner honoured requirement %q, which is not in %v", name, Requirements)
		}
	}
}

func TestRequirementValidationRefusesDuplicatesAndOverlongLists(t *testing.T) {
	value := runRecipe()
	value.Requires = []string{RequiresProfileSession, RequiresProfileSession}
	if err := Validate(value); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a duplicated requirement = %v, want it refused", err)
	}
	value.Requires = slices.Repeat([]string{RequiresProfileSession}, maxRequirements+1)
	err := Validate(value)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("an overlong requirement list = %v, want it refused by the bound", err)
	}
}

// Adding the field must not have moved an existing recipe's digest: a recipe
// published before this change is pinned by digest, and a digest that shifted
// would make every one of them unfetchable.
func TestRequiresIsOmittedFromARecipeThatDeclaresNone(t *testing.T) {
	value := runRecipe()
	value.Requires = nil
	digestWithout, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	value.Requires = []string{}
	digestWithEmpty, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	if digestWithout != digestWithEmpty {
		t.Fatalf("an empty requires list moved the digest: %s vs %s", digestWithout, digestWithEmpty)
	}
	value.Requires = []string{RequiresProfileSession}
	digestWith, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	if digestWith == digestWithout {
		t.Fatal("declaring a requirement did not change the digest, so the declaration is not covered by the pin")
	}
}

// BrowserSurface must not answer on a controller's behalf. It is the only layer
// that can tell "this transport says no" from "this transport has no opinion",
// and collapsing the two is how a fail-closed check becomes fail-open.
func TestBrowserSurfaceForwardsTheQuestionRatherThanAnsweringIt(t *testing.T) {
	// A controller implementing nothing optional: the compatibility shape the
	// Surface exists to tolerate elsewhere.
	surface := &BrowserSurface{Browser: opinionlessController{}}
	err := surface.CheckProfileSession()
	if err == nil {
		t.Fatal("BrowserSurface answered yes for a controller that never said so")
	}
	if !strings.Contains(err.Error(), "does not report") {
		t.Fatalf("refusal = %v, want it to say the transport does not report", err)
	}
}

// opinionlessController implements browser.Controller and nothing optional.
type opinionlessController struct{ browser.Controller }

var _ browser.Controller = opinionlessController{}
