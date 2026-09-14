package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Don-Works/brw/internal/recipe"
)

// compilePlanEnvURL and compilePlanEnvToken configure the private provider's
// write API. They are environment variables rather than flags because a token
// on a command line is a token in the shell history and in every process
// listing on the machine.
const (
	compilePlanEnvURL   = "BRW_RECIPE_PROVIDER_URL"
	compilePlanEnvToken = "BRW_RECIPE_PROVIDER_TOKEN" //nolint:gosec // names the variable, holds nothing
)

// recipeCompile turns a scoped trace into a reviewed recipe.
//
// Nothing is written until the whole trace has compiled. A trace carrying a
// credential field, a coordinate click, an ambiguous target, an undeclared
// origin or a step with nothing observed after it fails here, with no file on
// disk to redact afterwards — a draft that was written and then cleaned up is a
// draft that existed, and on a shared machine that is the whole exposure.
func recipeCompile(fromTrace, planPath, out string, publish bool) error {
	planData, err := readRecipeSource(planPath, false)
	if err != nil {
		return err
	}
	options, err := decodeCompilePlan(planData)
	if err != nil {
		return err
	}
	traceData, err := readRecipeSource(fromTrace, false)
	if err != nil {
		return err
	}
	steps, err := decodeTraceSteps(traceData)
	if err != nil {
		return err
	}
	destination, err := draftDestination(out)
	if err != nil {
		return err
	}

	result, err := recipe.Compile(steps, options)
	if err != nil {
		return err
	}
	draft := recipe.NewDraft(result, "brw_trace")
	if err := recipe.ValidateDraft(draft); err != nil {
		return err
	}

	// The review body goes to stdout every time. It is the artifact a human
	// actually reviews, and a compile whose only output was a JSON file would
	// be reviewed by nobody.
	fmt.Print(result.Review)

	if destination != "" {
		body, err := json.MarshalIndent(result.Recipe, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(destination, append(body, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote draft %s\n", destination)
	}
	if publish {
		published, err := publishDraft(draft)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "published draft %s@%s digest %s\n", published.ID, published.Version, published.Digest)
	}
	if destination == "" && !publish {
		fmt.Fprintln(os.Stderr, "review only: pass --out <path outside every git checkout> or --publish to keep this draft")
	}
	return nil
}

// draftDestination refuses any path inside a Git checkout before compilation
// begins, so a refusal cannot arrive after a file already exists.
//
// This repository is a Git checkout, so the rule alone keeps a draft out of it;
// the check is written against every checkout rather than against this one
// because an operator's private recipes do not belong in their other
// repositories either.
func draftDestination(out string) (string, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if err := recipe.EnsureOutsideGitCheckout(absolute); err != nil {
		return "", fmt.Errorf("refusing to write a draft to %s: %w", absolute, err)
	}
	return absolute, nil
}

func publishDraft(draft recipe.Draft) (recipe.PublishedDraft, error) {
	baseURL := strings.TrimSpace(os.Getenv(compilePlanEnvURL))
	if baseURL == "" {
		return recipe.PublishedDraft{}, fmt.Errorf("--publish needs the private provider's write API in %s", compilePlanEnvURL)
	}
	provider, err := recipe.NewHTTPProvider(recipe.HTTPProviderConfig{
		BaseURL: baseURL,
		Token:   os.Getenv(compilePlanEnvToken),
	})
	if err != nil {
		return recipe.PublishedDraft{}, err
	}
	return provider.PublishDraft(context.Background(), draft)
}

// decodeCompilePlan reads the operator's declarations: identity, origins, risk
// and which steps commit external writes. Unknown fields are rejected, because
// a plan whose "writes" key is misspelled would otherwise compile every write
// in the flow as a read.
func decodeCompilePlan(data []byte) (recipe.CompileOptions, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var options recipe.CompileOptions
	if err := decoder.Decode(&options); err != nil {
		return recipe.CompileOptions{}, fmt.Errorf("parse compile plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return recipe.CompileOptions{}, errors.New("compile plan contains trailing JSON")
	}
	return options, nil
}

// decodeTraceSteps accepts a bare array of scoped trace steps, or the object a
// scoped trace buffer is served as.
func decodeTraceSteps(data []byte) ([]recipe.TraceStep, error) {
	var direct []recipe.TraceStep
	if err := json.Unmarshal(data, &direct); err == nil && len(direct) > 0 {
		return direct, nil
	}
	var wrapped struct {
		Steps   []recipe.TraceStep `json:"steps"`
		Entries []recipe.TraceStep `json:"entries"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse trace JSON: %w", err)
	}
	if len(wrapped.Steps) > 0 {
		return wrapped.Steps, nil
	}
	if len(wrapped.Entries) > 0 {
		return wrapped.Entries, nil
	}
	return nil, errors.New("trace JSON contained no steps")
}
