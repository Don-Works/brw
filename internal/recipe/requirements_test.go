package recipe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

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

	if surface.fills != 0 || surface.clicks != 0 || surface.navigations != 0 {
		t.Fatalf("the recipe touched the browser before refusing: %+v", surface)
	}
}

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

func TestEveryDeclaredRequirementIsValidatedAndHonoured(t *testing.T) {
	for _, name := range Requirements {
		t.Run(name, func(t *testing.T) {
			value := runRecipe()
			value.Requires = []string{name}
			if err := Validate(value); err != nil {
				t.Fatalf("the schema refuses the declared requirement %q: %v", name, err)
			}

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

func TestBrowserSurfaceForwardsTheQuestionRatherThanAnsweringIt(t *testing.T) {

	surface := &BrowserSurface{Browser: opinionlessController{}}
	err := surface.CheckProfileSession()
	if err == nil {
		t.Fatal("BrowserSurface answered yes for a controller that never said so")
	}
	if !strings.Contains(err.Error(), "does not report") {
		t.Fatalf("refusal = %v, want it to say the transport does not report", err)
	}
}

type opinionlessController struct{ browser.Controller }

var _ browser.Controller = opinionlessController{}
