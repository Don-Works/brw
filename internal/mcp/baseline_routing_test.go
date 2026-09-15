package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/recipe"
)

// recordingBaselines is a provider stand-in. It records what it was asked and
// keeps whatever it was given, so a test can say which store a capture landed
// in rather than inferring it from a status string.
type recordingBaselines struct {
	owned     map[string]bool
	records   map[string]baseline.Record
	ownsError error
	asked     []string
}

func newRecordingBaselines(owned ...string) *recordingBaselines {
	store := &recordingBaselines{owned: map[string]bool{}, records: map[string]baseline.Record{}}
	for _, digest := range owned {
		store.owned[strings.ToLower(digest)] = true
	}
	return store
}

func (r *recordingBaselines) OwnsRecipe(_ context.Context, digest string) (bool, error) {
	r.asked = append(r.asked, digest)
	if r.ownsError != nil {
		return false, r.ownsError
	}
	return r.owned[strings.ToLower(digest)], nil
}

func (r *recordingBaselines) PutBaseline(_ context.Context, record baseline.Record) error {
	if err := record.Key.Validate(); err != nil {
		return err
	}
	r.records[record.Key.ID()] = record
	return nil
}

func (r *recordingBaselines) LoadBaseline(_ context.Context, key baseline.Key) (baseline.Record, bool, error) {
	record, ok := r.records[key.ID()]
	return record, ok, nil
}

func (r *recordingBaselines) BaselineEnvironments(_ context.Context, digest string, step int) ([]baseline.Environment, error) {
	var out []baseline.Environment
	for _, record := range r.records {
		if strings.EqualFold(record.Key.RecipeDigest, digest) && record.Key.StepIndex == step {
			out = append(out, record.Environment)
		}
	}
	return out, nil
}

func (r *recordingBaselines) DeleteBaseline(_ context.Context, key baseline.Key) error {
	if _, ok := r.records[key.ID()]; !ok {
		return errors.New("no baseline stored for that key")
	}
	delete(r.records, key.ID())
	return nil
}

func (r *recordingBaselines) BaselineLocation() string { return "the private recipe provider (test)" }

var _ recipe.BaselineStore = (*recordingBaselines)(nil)

// isToolError reports whether a tool result is the refusal envelope, and
// returns the message with it so a test can say which refusal it got.
func isToolError(t *testing.T, result any) (bool, string) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	message := ""
	if len(envelope.Content) > 0 {
		message = envelope.Content[0].Text
	}
	return envelope.IsError, message
}

// localBaselineFiles counts the baseline records written under a local root, so
// "it did not go to the local store" is checked against the filesystem rather
// than against a label in the result.
func localBaselineFiles(t *testing.T, root string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "*", "step-*", "*", "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// TestBaselineForAProviderOwnedRecipeGoesToTheProvider is T3B4JJ's routing
// rule: a baseline of a page a private recipe reached is stored with the
// provider that owns that recipe, and the local root is left for the ones it
// does not own.
func TestBaselineForAProviderOwnedRecipeGoesToTheProvider(t *testing.T) {
	// A second digest, for a recipe the provider does not hold: the public
	// fixture case, which must still land in the local root.
	const publicDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	tests := []struct {
		name         string
		digest       string
		wantProvider bool
	}{
		{name: "a recipe the provider owns", digest: fixtureBaselineDigest, wantProvider: true},
		{name: "a public fixture the provider does not own", digest: publicDigest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
			s := baselineServer(t, controller)
			provider := newRecordingBaselines(fixtureBaselineDigest)
			s.SetRecipeBaselines(provider)
			localRoot := s.baselines.Root()

			args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":1}`, tc.digest)
			result := callBaselineTool(t, s, args)
			if status, _ := result["status"].(string); status != baseline.StatusRecorded {
				t.Fatalf("update status = %v, want recorded", result["status"])
			}
			storedIn, _ := result["stored_in"].(string)
			if len(provider.asked) == 0 || provider.asked[0] != tc.digest {
				t.Fatalf("the provider was asked %v, want the digest being gated", provider.asked)
			}

			if tc.wantProvider {
				if len(provider.records) != 1 {
					t.Fatalf("the provider holds %d baselines, want the one just recorded", len(provider.records))
				}
				if got := localBaselineFiles(t, localRoot); got != 0 {
					t.Fatalf("%d baselines were written to the local root %s for a provider-owned recipe", got, localRoot)
				}
				if !strings.Contains(storedIn, "provider") {
					t.Fatalf("stored_in = %q, which does not name the provider", storedIn)
				}
			} else {
				if len(provider.records) != 0 {
					t.Fatalf("a public fixture's baseline was sent to the private provider: %+v", provider.records)
				}
				if got := localBaselineFiles(t, localRoot); got != 1 {
					t.Fatalf("%d baselines under the local root, want the one just recorded", got)
				}
				if !strings.Contains(storedIn, localRoot) {
					t.Fatalf("stored_in = %q, which does not name the local root %s", storedIn, localRoot)
				}
			}

			// The check that follows must read back from the same place: a
			// baseline written to one store and compared against the other is a
			// gate that never fires.
			checked := callBaselineTool(t, s, fmt.Sprintf(`{"action":"check","recipe_digest":%q,"step_index":1}`, tc.digest))
			if status, _ := checked["status"].(string); status != baseline.StatusMatch {
				t.Fatalf("check after update = %v, want match", checked["status"])
			}

			// And so must delete, or an operator who drops a baseline is told it
			// worked while the stored one keeps gating.
			deleted := callBaselineTool(t, s, fmt.Sprintf(`{"action":"delete","recipe_digest":%q,"step_index":1}`, tc.digest))
			if note, _ := deleted["note"].(string); note != "baseline deleted" {
				t.Fatalf("delete = %+v", deleted)
			}
			if len(provider.records) != 0 || localBaselineFiles(t, localRoot) != 0 {
				t.Fatalf("delete left %d provider records and %d local records", len(provider.records), localBaselineFiles(t, localRoot))
			}
		})
	}
}

// TestBaselineRoutingRefusesRatherThanFallingBackToTheLocalRoot: the fallback
// would put a screenshot of a private page in the local root at exactly the
// moment the provider cannot be reached to say it is private.
func TestBaselineRoutingRefusesRatherThanFallingBackToTheLocalRoot(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
	s := baselineServer(t, controller)
	provider := newRecordingBaselines(fixtureBaselineDigest)
	provider.ownsError = errors.New("the provider is unreachable")
	s.SetRecipeBaselines(provider)
	localRoot := s.baselines.Root()

	args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":1}`, fixtureBaselineDigest)
	result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	failed, message := isToolError(t, result)
	if !failed {
		t.Fatalf("an unreachable provider did not fail the call: %+v", result)
	}
	if !strings.Contains(message, "unreachable") {
		t.Fatalf("refusal = %q, which does not say the provider could not be asked", message)
	}
	if got := localBaselineFiles(t, localRoot); got != 0 {
		t.Fatalf("%d baselines fell back to the local root when the provider could not be asked", got)
	}
}

// TestBaselinesWorkWithAProviderAndNoLocalRoot: a deployment whose recipes all
// come from the provider should not have to configure a local root it never
// writes to.
func TestBaselinesWorkWithAProviderAndNoLocalRoot(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
	s := New(controller)
	provider := newRecordingBaselines(fixtureBaselineDigest)
	s.SetRecipeBaselines(provider)

	result := callBaselineTool(t, s, fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, fixtureBaselineDigest))
	if status, _ := result["status"].(string); status != baseline.StatusRecorded {
		t.Fatalf("update with no local root = %+v", result)
	}
	if len(provider.records) != 1 {
		t.Fatalf("the provider holds %d baselines", len(provider.records))
	}

	// A recipe the provider does not own still has nowhere to go, and says so.
	const publicDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, publicDigest)
	unowned, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	failed, message := isToolError(t, unowned)
	if !failed {
		t.Fatalf("a daemon with no local root recorded a baseline it has nowhere to put: %+v", unowned)
	}
	if !strings.Contains(message, "--baseline-root") {
		t.Fatalf("refusal = %q, which does not name the flag that would fix it", message)
	}
}
