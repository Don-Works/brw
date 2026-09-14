package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/credential"
	"github.com/Don-Works/brw/internal/plugin"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/usagelog"
)

// Low-entropy and obviously fabricated. The whole test is a search for this
// literal, so it must not look like a real credential.
const (
	leakFixtureValue     = "fixture-login-value-one"
	leakFixtureReference = "work/login"
	leakFixtureOrigin    = "https://login.example.test"
)

// credentialFillSurface is the browser side. It keeps the typed value behind a
// mutex-guarded field the test reads directly, because here the assertion is
// about the MCP transcript and the ledger, not about the daemon's heap.
type credentialFillSurface struct {
	mu      sync.Mutex
	typed   []string
	fillErr error
}

func (s *credentialFillSurface) Origin(context.Context) (string, error) {
	return leakFixtureOrigin, nil
}

func (s *credentialFillSurface) Resolve(_ context.Context, target recipe.Target) ([]recipe.ResolvedElement, error) {
	return []recipe.ResolvedElement{{Ref: "e1", Role: target.Role, Name: target.Name}}, nil
}

func (s *credentialFillSurface) Click(context.Context, string) error { return nil }

func (s *credentialFillSurface) Fill(_ context.Context, _ string, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.typed = append(s.typed, value)
	return s.fillErr
}

func (s *credentialFillSurface) Type(context.Context, string, string) error   { return nil }
func (s *credentialFillSurface) Select(context.Context, string, string) error { return nil }
func (s *credentialFillSurface) Press(context.Context, string, string) error  { return nil }
func (s *credentialFillSurface) NavigateTo(context.Context, string) error     { return nil }
func (s *credentialFillSurface) WaitEvent(context.Context, recipe.Event) error {
	return nil
}

func (s *credentialFillSurface) Capture(context.Context, recipe.CaptureSpec) (artifact.Meta, error) {
	return artifact.Meta{ID: "art_00000000000000000000000000000000", Kind: "text"}, nil
}

func (s *credentialFillSurface) filled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.typed...)
}

type oneRecipeProvider struct{ value recipe.Recipe }

func (p oneRecipeProvider) Search(context.Context, string, string, int) ([]recipe.Match, error) {
	return nil, nil
}

func (p oneRecipeProvider) Fetch(context.Context, string, string, string) (recipe.Recipe, error) {
	return p.value, nil
}

func credentialLoginRecipe() recipe.Recipe {
	return recipe.Recipe{
		SchemaVersion: recipe.SchemaVersion,
		ID:            "example.login.sign-in",
		Version:       "1.0.0",
		Name:          "Sign in",
		Description:   "Sign in to the fixture login portal.",
		Intents:       []string{"sign in to the portal"},
		Origins:       []string{leakFixtureOrigin},
		Risk:          "read_only",
		Steps: []recipe.Step{
			{ID: "password", Action: "fill", Effect: "read",
				Target: &recipe.Target{Role: "textbox", Name: "Password"},
				Value:  credential.Scheme + leakFixtureReference},
		},
	}
}

func credentialLeakRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	root, credentials := t.TempDir(), t.TempDir()
	path := filepath.Join(credentials, filepath.FromSlash(leakFixtureReference))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(leakFixtureValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"schema_version": plugin.ManifestSchemaVersion,
		"id":             "local.files",
		"name":           "Local credential files",
		"version":        "1.0.0",
		"description":    "One file per credential",
		"capabilities":   []string{plugin.CapabilityCredentialRead},
		"credential":     map[string]any{"kind": plugin.CredentialKindFile, "directory": credentials},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugin.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// Acceptance criterion 2, over the wire rather than over an internal API: a
// login completes and the value appears in neither the MCP transcript the
// client receives nor the operational ledger the daemon writes.
func TestCredentialNeverEntersTheMCPTranscriptOrTheUsageLedger(t *testing.T) {
	value := credentialLoginRecipe()
	digest, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	surface := &credentialFillSurface{}
	service, err := recipe.NewService(oneRecipeProvider{value: value}, recipe.Runner{
		Surface: surface, Credentials: credentialLeakRegistry(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	ledger := filepath.Join(t.TempDir(), "usage.jsonl")
	recorder, err := usagelog.New(usagelog.Config{Path: ledger, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	server := New(fakeController{})
	server.SetRecipeAPI(service)
	server.SetUsageRecorder(recorder)

	input := framedJSON(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}) +
		framedJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{
				"name": "brw_recipe_run",
				"arguments": map[string]any{
					"id": value.ID, "version": value.Version, "digest": digest,
				},
			},
		})
	var transcript bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &transcript); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	// The flow has to have worked, or the searches below prove nothing.
	if got := surface.filled(); len(got) != 1 || got[0] != leakFixtureValue {
		t.Fatalf("the browser received %q, want the resolved credential", got)
	}
	if !bytes.Contains(transcript.Bytes(), []byte(`"status":"done"`)) {
		t.Fatalf("the recipe run did not complete: %s", transcript.String())
	}

	if bytes.Contains(transcript.Bytes(), []byte(leakFixtureValue)) {
		t.Fatalf("the MCP transcript carries the credential: %s", transcript.String())
	}
	recorded, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) == 0 {
		t.Fatal("the ledger recorded nothing; a search over an empty file proves nothing")
	}
	if bytes.Contains(recorded, []byte(leakFixtureValue)) {
		t.Fatalf("the usage ledger carries the credential: %s", recorded)
	}
	// The reference name is not secret, but it names an operator's vault layout
	// and the ledger is metadata-only, so it has no business being there either.
	if bytes.Contains(recorded, []byte(leakFixtureReference)) {
		t.Fatalf("the usage ledger carries the credential reference: %s", recorded)
	}
}

// The realistic way a credential reaches an MCP client is not a field brw
// chose to include: it is a transport quoting the text it could not type, into
// an error brw forwards. This drives that path over the tool call.
func TestAChattyFillFailureDoesNotPutTheCredentialInTheMCPResponse(t *testing.T) {
	value := credentialLoginRecipe()
	digest, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	surface := &credentialFillSurface{fillErr: fmt.Errorf("could not set field to %q", leakFixtureValue)}
	service, err := recipe.NewService(oneRecipeProvider{value: value}, recipe.Runner{
		Surface: surface, Credentials: credentialLeakRegistry(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(fakeController{})
	server.SetRecipeAPI(service)

	result, rpcErr := server.callTool(context.Background(), "brw_recipe_run", json.RawMessage(
		`{"id":"`+value.ID+`","version":"`+value.Version+`","digest":"`+digest+`"}`))
	if rpcErr != nil {
		t.Fatalf("tool call failed at the protocol level: %+v", rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	// The transport really did see the value, so the search below is not over an
	// error that never had one to leak.
	if got := surface.filled(); len(got) != 1 || got[0] != leakFixtureValue {
		t.Fatalf("the browser received %q", got)
	}
	if bytes.Contains(encoded, []byte(leakFixtureValue)) {
		t.Fatalf("the MCP response carries the credential: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(credential.Placeholder)) {
		t.Fatalf("the MCP response %s does not show the value was withheld", encoded)
	}
}

// Criterion 3 over the wire: with the provider revoked, the tool call fails
// with the named error instead of degrading to an empty field.
func TestRevokedProviderFailsTheRecipeToolCallByName(t *testing.T) {
	value := credentialLoginRecipe()
	digest, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	surface := &credentialFillSurface{}
	registry := credentialLeakRegistry(t)
	service, err := recipe.NewService(oneRecipeProvider{value: value}, recipe.Runner{
		Surface: surface, Credentials: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Revoke("local.files"); err != nil {
		t.Fatal(err)
	}

	server := New(fakeController{})
	server.SetRecipeAPI(service)
	result, rpcErr := server.callTool(context.Background(), "brw_recipe_run", json.RawMessage(
		`{"id":"`+value.ID+`","version":"`+value.Version+`","digest":"`+digest+`"}`))
	if rpcErr != nil {
		t.Fatalf("tool call failed at the protocol level: %+v", rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("revoked")) || !bytes.Contains(encoded, []byte("local.files")) {
		t.Fatalf("the revoked run did not name the refusal: %s", encoded)
	}
	if got := surface.filled(); len(got) != 0 {
		t.Fatalf("a revoked run still typed %q", got)
	}
}
