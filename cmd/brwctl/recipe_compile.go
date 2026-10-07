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

const (
	compilePlanEnvURL   = "BRW_RECIPE_PROVIDER_URL"
	compilePlanEnvToken = "BRW_RECIPE_PROVIDER_TOKEN" //nolint:gosec // names the variable, holds nothing
)

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

	fmt.Print(result.Review)

	if destination != "" {
		body, err := json.MarshalIndent(result.Recipe, "", "  ")
		if err != nil {
			return err
		}
		if err := writeDraftFile(destination, append(body, '\n')); err != nil {
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

func writeDraftFile(destination string, body []byte) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create draft %s: %w", destination, err)
	}
	abandon := func(cause error) error {
		file.Close()
		os.Remove(destination)
		return cause
	}
	if err := recipe.EnsureOutsideGitCheckout(destination); err != nil {
		return abandon(fmt.Errorf("refusing to write a draft to %s: %w", destination, err))
	}
	if _, err := file.Write(body); err != nil {
		return abandon(err)
	}
	if err := file.Close(); err != nil {
		os.Remove(destination)
		return err
	}
	return nil
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

func decodeCompilePlan(data []byte) (recipe.CompileOptions, error) {
	var options recipe.CompileOptions
	if err := decodeStrict(data, &options); err != nil {
		return recipe.CompileOptions{}, fmt.Errorf("parse compile plan: %w", err)
	}
	return options, nil
}

type traceStepEnvelope struct {
	recipe.TraceStep
	TabID      string `json:"tab_id,omitempty"`
	Repeat     int    `json:"repeat,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

func decodeTraceSteps(data []byte) ([]recipe.TraceStep, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("trace JSON is empty")
	}
	if trimmed[0] == '[' {
		var direct []traceStepEnvelope
		if err := decodeStrict(trimmed, &direct); err != nil {
			return nil, fmt.Errorf("parse trace JSON: %w", err)
		}
		return unwrapTraceSteps(direct)
	}
	var wrapped struct {
		Steps    []traceStepEnvelope `json:"steps"`
		Entries  []traceStepEnvelope `json:"entries"`
		Count    int                 `json:"count"`
		Withheld int                 `json:"withheld"`
	}
	if err := decodeStrict(trimmed, &wrapped); err != nil {
		return nil, fmt.Errorf("parse trace JSON: %w", err)
	}
	if len(wrapped.Steps) > 0 {
		return unwrapTraceSteps(wrapped.Steps)
	}
	return unwrapTraceSteps(wrapped.Entries)
}

func unwrapTraceSteps(envelopes []traceStepEnvelope) ([]recipe.TraceStep, error) {
	if len(envelopes) == 0 {
		return nil, errors.New("trace JSON contained no steps")
	}
	steps := make([]recipe.TraceStep, 0, len(envelopes))
	for _, envelope := range envelopes {
		steps = append(steps, envelope.TraceStep)
	}
	return steps, nil
}

func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON after the document")
	}
	return nil
}
