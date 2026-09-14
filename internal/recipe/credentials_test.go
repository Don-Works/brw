package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/credential"
	"github.com/Don-Works/brw/internal/plugin"
)

// Low-entropy and obviously fabricated: every non-leakage assertion below hunts
// for this exact literal, so it must never resemble a real credential.
const (
	fixtureCredentialValue = "fixture-login-value-one"
	fixtureReference       = "work/login"
	fixtureLoginOrigin     = "https://login.example.test"
)

// credentialSurface is the BROWSER side of the deterministic boundary.
//
// Everything it keeps in a field is state the daemon holds in production: the
// trace lives in the manager or the bridge, and the other recorded arguments
// are what brw sent browser-ward. The one thing it does not keep in a field is
// the text it was told to type, which goes to a func the test owns — a func is
// unreachable from a reflect walk, which models the truth that the page's copy
// of a typed password lives in another process entirely.
type credentialSurface struct {
	mu        sync.Mutex
	origin    string
	elements  []ResolvedElement
	events    map[string]bool
	record    func(action, value string)
	trace     []browser.TraceEntry
	otherArgs []string
	failOnRef string
	fillErr   error
	bundles   []FailureBundleRequest
	bundleID  string
}

func newCredentialSurface(record func(action, value string)) *credentialSurface {
	return &credentialSurface{
		origin: fixtureLoginOrigin,
		record: record,
		events: map[string]bool{},
		elements: []ResolvedElement{
			{Ref: "e1", Role: "textbox", Name: "Email"},
			{Ref: "e2", Role: "textbox", Name: "Password"},
			{Ref: "e3", Role: "button", Name: "Sign in"},
		},
	}
}

func (s *credentialSurface) Origin(context.Context) (string, error) { return s.origin, nil }

func (s *credentialSurface) Resolve(_ context.Context, target Target) ([]ResolvedElement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.otherArgs = append(s.otherArgs, target.Role, target.Name, target.NameContains, target.TestID, target.HrefContains)
	var out []ResolvedElement
	for _, element := range s.elements {
		if element.Role == target.Role && (target.Name == "" || element.Name == target.Name) {
			out = append(out, element)
		}
	}
	return out, nil
}

func (s *credentialSurface) Click(_ context.Context, ref string) error {
	s.mu.Lock()
	s.otherArgs = append(s.otherArgs, ref)
	s.events["text.present\x00Signed in"] = true
	s.mu.Unlock()
	return nil
}

func (s *credentialSurface) Fill(ctx context.Context, ref, value string) error {
	return s.typed(ctx, "fill", ref, value)
}

func (s *credentialSurface) Type(ctx context.Context, ref, value string) error {
	return s.typed(ctx, "type", ref, value)
}

// typed mirrors what both production transports do: hand the value to the page,
// then write a trace entry through browser.RedactTraceEntry with the context
// the caller built. The redaction under test is the real one.
func (s *credentialSurface) typed(ctx context.Context, action, ref, value string) error {
	s.mu.Lock()
	s.trace = append(s.trace, browser.RedactTraceEntry(ctx, browser.TraceEntry{
		Action: action, Ref: ref, Text: value, OK: true,
	}))
	record := s.record
	var err error
	if s.failOnRef == ref {
		err = s.fillErr
	}
	s.mu.Unlock()
	if record != nil {
		record(action, value)
	}
	return err
}

func (s *credentialSurface) Select(_ context.Context, ref, value string) error {
	s.mu.Lock()
	s.otherArgs = append(s.otherArgs, ref, value)
	s.mu.Unlock()
	return nil
}

func (s *credentialSurface) Press(_ context.Context, ref, key string) error {
	s.mu.Lock()
	s.otherArgs = append(s.otherArgs, ref, key)
	s.mu.Unlock()
	return nil
}

func (s *credentialSurface) NavigateTo(_ context.Context, url string) error {
	s.mu.Lock()
	s.otherArgs = append(s.otherArgs, url)
	s.mu.Unlock()
	return nil
}

func (s *credentialSurface) WaitEvent(ctx context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.otherArgs = append(s.otherArgs, event.Kind, event.Match)
	s.trace = append(s.trace, browser.RedactTraceEntry(ctx, browser.TraceEntry{Action: "wait", Text: event.Match, OK: true}))
	if s.events[event.Kind+"\x00"+event.Match] {
		return nil
	}
	return fmt.Errorf("event %q never happened", event.Kind)
}

func (s *credentialSurface) Capture(_ context.Context, spec CaptureSpec) (artifact.Meta, error) {
	s.mu.Lock()
	s.otherArgs = append(s.otherArgs, spec.Kind, spec.Ref, spec.Redaction, spec.Filename)
	s.mu.Unlock()
	return artifact.Meta{ID: "art_00000000000000000000000000000000", Kind: spec.Kind}, nil
}

func (s *credentialSurface) CaptureFailureBundle(_ context.Context, req FailureBundleRequest) (artifact.Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles = append(s.bundles, req)
	return artifact.Meta{ID: s.bundleID, Kind: "failure_bundle"}, nil
}

func loginRecipe(value string) Recipe {
	recipe := validRecipe(fixtureLoginOrigin)
	recipe.ID = "example.login.sign-in"
	recipe.Inputs = map[string]Input{"user": {Required: true}}
	recipe.Steps = []Step{
		{ID: "email", Action: "fill", Effect: "read", Target: &Target{Role: "textbox", Name: "Email"}, Value: "${input:user}"},
		{ID: "password", Action: "fill", Effect: "read", Target: &Target{Role: "textbox", Name: "Password"}, Value: value},
		{ID: "submit", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Sign in"},
			Postcondition: &Event{Kind: "text.present", Match: "Signed in", TimeoutMS: 500}},
	}
	return recipe
}

// credentialRegistry builds the reference file provider holding one fixture
// credential, so the whole flow runs on a machine with no vault CLI.
func credentialRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	root, credentials := t.TempDir(), t.TempDir()
	path := filepath.Join(credentials, filepath.FromSlash(fixtureReference))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fixtureCredentialValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema_version": plugin.ManifestSchemaVersion,
		"id":             "local.files",
		"name":           "Local credential files",
		"version":        "1.0.0",
		"description":    "One file per credential",
		"capabilities":   []string{plugin.CapabilityCredentialRead},
		"credential":     map[string]any{"kind": plugin.CredentialKindFile, "directory": credentials},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugin.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// typedRecorder stands in for the browser process. Nothing in the daemon's
// object graph points at it.
type typedRecorder struct {
	mu     sync.Mutex
	values []string
}

func (r *typedRecorder) record(_, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, value)
}

func (r *typedRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.values...)
}

func TestCredentialReachesTheFillAndNothingElseThatIsWrittenDown(t *testing.T) {
	typed := &typedRecorder{}
	surface := newCredentialSurface(typed.record)
	registry := credentialRegistry(t)
	recipe := loginRecipe(credential.Scheme + fixtureReference)

	result, err := (Runner{Surface: surface, Credentials: registry}).
		Run(context.Background(), recipe, map[string]string{"user": "fixture-user-one"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" {
		t.Fatalf("run status = %q, steps %+v", result.Status, result.Steps)
	}

	// The feature has to actually work: the page was given the real value.
	if got := typed.all(); len(got) != 2 || got[1] != fixtureCredentialValue {
		t.Fatalf("browser received %q, want the resolved credential in the password field", got)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(fixtureCredentialValue)) {
		t.Fatalf("run result carries the credential: %s", encoded)
	}
	for _, argument := range surface.otherArgs {
		if strings.Contains(argument, fixtureCredentialValue) {
			t.Fatalf("brw sent the credential to a non-fill browser call: %q", argument)
		}
	}

	var credentialEntries int
	for _, entry := range surface.trace {
		if strings.Contains(entry.Text, fixtureCredentialValue) || strings.Contains(entry.Value, fixtureCredentialValue) {
			t.Fatalf("trace entry %+v carries the credential", entry)
		}
		if entry.CredentialSourced {
			credentialEntries++
			if !entry.Redacted || entry.Text != "" {
				t.Fatalf("credential-sourced trace entry %+v was not redacted", entry)
			}
		}
	}
	if credentialEntries != 1 {
		t.Fatalf("trace marked %d entries credential-sourced, want exactly the password fill", credentialEntries)
	}
}

func TestCredentialIsScrubbedFromTheErrorResultAndFailureBundle(t *testing.T) {
	typed := &typedRecorder{}
	surface := newCredentialSurface(typed.record)
	// A chatty transport quoting the text it could not type is the realistic
	// path from a failed fill into the result, the response and the bundle.
	surface.failOnRef = "e2"
	surface.fillErr = fmt.Errorf("could not set field to %q", fixtureCredentialValue)
	surface.bundleID = "art_11111111111111111111111111111111"
	registry := credentialRegistry(t)
	recipe := loginRecipe(credential.Scheme + fixtureReference)
	recipe.CaptureOnFailure = true

	result, err := (Runner{Surface: surface, Credentials: registry}).
		Run(context.Background(), recipe, map[string]string{"user": "fixture-user-one"})
	if err == nil {
		t.Fatal("a failing credential fill reported success")
	}
	if strings.Contains(err.Error(), fixtureCredentialValue) {
		t.Fatalf("run error carries the credential: %v", err)
	}
	if !strings.Contains(err.Error(), credential.Placeholder) {
		t.Fatalf("run error %q does not show the value was withheld", err)
	}
	for chain := err; chain != nil; chain = errors.Unwrap(chain) {
		if strings.Contains(chain.Error(), fixtureCredentialValue) {
			t.Fatalf("an error in the chain carries the credential: %v", chain)
		}
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(encoded, []byte(fixtureCredentialValue)) {
		t.Fatalf("failed run result carries the credential: %s", encoded)
	}
	if len(surface.bundles) != 1 {
		t.Fatalf("collected %d failure bundles, want 1", len(surface.bundles))
	}
	if strings.Contains(surface.bundles[0].Reason, fixtureCredentialValue) {
		t.Fatalf("failure bundle reason carries the credential: %q", surface.bundles[0].Reason)
	}
	if result.FailureBundle != surface.bundleID {
		t.Fatalf("result failure bundle = %q want %q", result.FailureBundle, surface.bundleID)
	}
}

func TestRevokingTheProviderFailsTheRunClosedBeforeAnythingIsTyped(t *testing.T) {
	registry := credentialRegistry(t)
	recipe := loginRecipe(credential.Scheme + fixtureReference)

	first := &typedRecorder{}
	if _, err := (Runner{Surface: newCredentialSurface(first.record), Credentials: registry}).
		Run(context.Background(), recipe, map[string]string{"user": "fixture-user-one"}); err != nil {
		t.Fatalf("the flow did not work before the revoke: %v", err)
	}

	if err := registry.Revoke("local.files"); err != nil {
		t.Fatal(err)
	}

	second := &typedRecorder{}
	_, err := (Runner{Surface: newCredentialSurface(second.record), Credentials: registry}).
		Run(context.Background(), recipe, map[string]string{"user": "fixture-user-one"})
	if !errors.Is(err, credential.ErrProviderRevoked) {
		t.Fatalf("run after revoke = %v, want ErrProviderRevoked", err)
	}
	if !strings.Contains(err.Error(), "local.files") {
		t.Fatalf("revoked run error %q does not name the plugin", err)
	}
	if got := second.all(); len(got) != 0 {
		t.Fatalf("a revoked run still typed %q; it must fail before its first side effect", got)
	}
}

func TestRunWithNoProviderFailsBeforeTouchingTheBrowser(t *testing.T) {
	recipe := loginRecipe(credential.Scheme + fixtureReference)
	for name, resolver := range map[string]credential.Resolver{
		"no resolver at all": nil,
		"empty registry":     plugin.Empty(),
	} {
		t.Run(name, func(t *testing.T) {
			typed := &typedRecorder{}
			_, err := (Runner{Surface: newCredentialSurface(typed.record), Credentials: resolver}).
				Run(context.Background(), recipe, map[string]string{"user": "fixture-user-one"})
			if !errors.Is(err, credential.ErrNoProvider) {
				t.Fatalf("run = %v, want ErrNoProvider", err)
			}
			if got := typed.all(); len(got) != 0 {
				t.Fatalf("the run typed %q before failing", got)
			}
		})
	}
}

// Acceptance criterion 4: after the fill returns, nothing the daemon retains
// holds the value. The walk reads unexported fields on purpose — the claim is
// about the daemon's heap, not about what it chooses to marshal.
func TestDaemonRetainsNoCredentialAfterTheFillReturns(t *testing.T) {
	typed := &typedRecorder{}
	surface := newCredentialSurface(typed.record)
	registry := credentialRegistry(t)
	recipe := loginRecipe(credential.Scheme + fixtureReference)
	runner := Runner{Surface: surface, Credentials: registry}
	service, err := NewService(stubProvider{recipe: recipe}, runner)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(recipe)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.RunRecipe(context.Background(), RunRequest{
		ID: recipe.ID, Version: recipe.Version, Digest: digest,
		Inputs: map[string]string{"user": "fixture-user-one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := typed.all(); len(got) != 2 || got[1] != fixtureCredentialValue {
		t.Fatalf("browser received %q; the walk below would be vacuous if the fill never carried the value", got)
	}

	// Everything brwd still owns once the run has returned.
	retained := struct {
		Registry *plugin.Registry
		Service  *Service
		Runner   Runner
		Result   RunResult
		Recipe   Recipe
		Trace    []browser.TraceEntry
	}{registry, service, runner, result, recipe, surface.trace}

	if hits := findRetained(reflect.ValueOf(retained), fixtureCredentialValue); len(hits) > 0 {
		t.Fatalf("the daemon still holds the credential at %v", hits)
	}
	// The walk has to be able to find one, or it proves nothing.
	if hits := findRetained(reflect.ValueOf(retained), fixtureReference); len(hits) == 0 {
		t.Fatal("the walk found no occurrence of the reference name either; it is not reaching the retained state")
	}
}

// retainingResolver keeps its own copy of the Secret it handed out. Copies of a
// Secret share one backing array by design, so this copy is the daemon-side
// view of the value after the step: it must read as zeroes.
type retainingResolver struct {
	value  string
	issued credential.Secret
}

func (r *retainingResolver) Resolve(context.Context, string) (credential.Secret, error) {
	r.issued = credential.New(r.value)
	return r.issued, nil
}

func TestRunnerWipesTheResolvedCredentialWhenTheStepReturns(t *testing.T) {
	typed := &typedRecorder{}
	resolver := &retainingResolver{value: fixtureCredentialValue}
	if _, err := (Runner{Surface: newCredentialSurface(typed.record), Credentials: resolver}).
		Run(context.Background(), loginRecipe(credential.Scheme+fixtureReference), map[string]string{"user": "fixture-user-one"}); err != nil {
		t.Fatal(err)
	}
	if got := typed.all(); len(got) != 2 || got[1] != fixtureCredentialValue {
		t.Fatalf("browser received %q; a wipe assertion on a value that was never used proves nothing", got)
	}
	// Wiping zeroes the array in place, so the surviving copy keeps its length
	// and loses its content. Checking the content is the only real check.
	if got := resolver.issued.Reveal(); got != strings.Repeat("\x00", len(fixtureCredentialValue)) {
		t.Fatalf("the credential survived the step as %q; the runner must wipe it when the fill returns", got)
	}
}

// findRetained reports the paths of every string or byte slice under root that
// contains needle, unexported fields included.
func findRetained(root reflect.Value, needle string) []string {
	var hits []string
	seen := map[uintptr]bool{}
	var walk func(value reflect.Value, path string, depth int)
	walk = func(value reflect.Value, path string, depth int) {
		if !value.IsValid() || depth > 32 {
			return
		}
		switch value.Kind() {
		case reflect.String:
			if strings.Contains(value.String(), needle) {
				hits = append(hits, path)
			}
		case reflect.Pointer:
			if value.IsNil() || seen[value.Pointer()] {
				return
			}
			seen[value.Pointer()] = true
			walk(value.Elem(), path, depth+1)
		case reflect.Interface:
			if value.IsNil() {
				return
			}
			walk(value.Elem(), path, depth+1)
		case reflect.Struct:
			for index := 0; index < value.NumField(); index++ {
				walk(value.Field(index), path+"."+value.Type().Field(index).Name, depth+1)
			}
		case reflect.Slice, reflect.Array:
			if value.Kind() == reflect.Slice {
				if value.IsNil() {
					return
				}
				if value.Type().Elem().Kind() == reflect.Uint8 {
					buffer := make([]byte, value.Len())
					for index := range buffer {
						buffer[index] = byte(value.Index(index).Uint())
					}
					if bytes.Contains(buffer, []byte(needle)) {
						hits = append(hits, path)
					}
					return
				}
				if seen[value.Pointer()] {
					return
				}
				seen[value.Pointer()] = true
			}
			for index := 0; index < value.Len(); index++ {
				walk(value.Index(index), fmt.Sprintf("%s[%d]", path, index), depth+1)
			}
		case reflect.Map:
			if value.IsNil() || seen[value.Pointer()] {
				return
			}
			seen[value.Pointer()] = true
			for _, key := range value.MapKeys() {
				walk(key, path+".<key>", depth+1)
				walk(value.MapIndex(key), fmt.Sprintf("%s[%v]", path, key), depth+1)
			}
		}
	}
	walk(root, "retained", 0)
	return hits
}

func TestAnInputCannotNameACredential(t *testing.T) {
	typed := &typedRecorder{}
	surface := newCredentialSurface(typed.record)
	recipe := loginRecipe("fixture-static-text")
	// The email step interpolates an input. A caller that hands it something
	// shaped like a reference must get that text typed, not a resolved secret.
	if _, err := (Runner{Surface: surface, Credentials: credentialRegistry(t)}).
		Run(context.Background(), recipe, map[string]string{"user": credential.Scheme + fixtureReference}); err != nil {
		t.Fatal(err)
	}
	got := typed.all()
	if len(got) != 2 || got[0] != credential.Scheme+fixtureReference {
		t.Fatalf("browser received %q, want the reference typed literally", got)
	}
	for _, value := range got {
		if value == fixtureCredentialValue {
			t.Fatal("an input expanded into a resolved credential; only the recipe may name one")
		}
	}
}

func TestValidateAllowsAReferenceOnlyAsAWholeFillOrTypeValue(t *testing.T) {
	reference := credential.Scheme + fixtureReference
	for name, test := range map[string]struct {
		mutate func(*Recipe)
		wantOK bool
	}{
		"whole fill value": {func(Recipe *Recipe) {}, true},
		"whole type value": {func(r *Recipe) { r.Steps[1].Action = "type" }, true},
		"select value":     {func(r *Recipe) { r.Steps[1].Action = "select" }, false},
		"inside a longer value": {
			func(r *Recipe) { r.Steps[1].Value = reference + " extra" }, false},
		"prefixed value": {
			func(r *Recipe) { r.Steps[1].Value = "prefix " + reference }, false},
		"target name": {
			func(r *Recipe) { r.Steps[0].Target.Name = reference }, false},
		"description": {
			func(r *Recipe) { r.Description = "Sign in with " + reference }, false},
		"metadata": {
			func(r *Recipe) { r.Metadata = map[string]string{"note": reference} }, false},
		"idempotency key": {
			func(r *Recipe) {
				r.Risk = "external_write"
				r.Steps[2].Effect = "external_write"
				r.Steps[2].IdempotencyKey = reference
			}, false},
		"event match": {
			func(r *Recipe) { r.Steps[2].Postcondition.Match = reference }, false},
		"navigate url": {
			func(r *Recipe) {
				r.Steps[2] = Step{ID: "go", Action: "navigate_to", Effect: "read", URL: fixtureLoginOrigin + "/" + reference}
			}, false},
		"assert value": {
			func(r *Recipe) {
				r.Steps[2] = Step{ID: "check", Action: "assert", Assert: &Assertion{
					Kind: "element_value", Target: &Target{Role: "textbox", Name: "Password"}, Expected: reference}}
			}, false},
		"malformed reference name": {
			func(r *Recipe) { r.Steps[1].Value = credential.Scheme + "work login" }, false},
		"empty reference name": {
			func(r *Recipe) { r.Steps[1].Value = credential.Scheme }, false},
		"two references in one value": {
			func(r *Recipe) { r.Steps[1].Value = reference + reference }, false},
	} {
		t.Run(name, func(t *testing.T) {
			recipe := loginRecipe(reference)
			test.mutate(&recipe)
			err := Validate(recipe)
			if test.wantOK && err != nil {
				t.Fatalf("Validate refused a legal recipe: %v", err)
			}
			if !test.wantOK {
				if err == nil {
					t.Fatal("Validate accepted a credential reference outside a fill or type value")
				}
				if !strings.Contains(err.Error(), credential.Scheme) {
					t.Fatalf("Validate error %q does not name the rule that refused it", err)
				}
			}
		})
	}
}

func TestParseAcceptsARecipeThatNamesACredential(t *testing.T) {
	recipe := loginRecipe(credential.Scheme + fixtureReference)
	encoded, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse refused a recipe naming a credential: %v", err)
	}
	if reference, ok := StepCredentialReference(parsed.Steps[1]); !ok || reference != fixtureReference {
		t.Fatalf("parsed step reference = %q,%v", reference, ok)
	}
}
