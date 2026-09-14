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

// writeDraftFile creates the draft without following a symlink and without
// overwriting anything that is already there.
//
// draftDestination checks the path, but a check on a path is a check on what
// that path resolved to at that moment: a dangling symlink at --out resolves to
// nothing, so it passes the check, and a plain write then follows it into
// whatever it names — a file inside this repository, for instance. O_EXCL
// refuses a symlink of any kind and refuses an existing file, so the name that
// was checked is the name that gets written, and the checkout rule is re-run
// against the created file's own resolved path before a byte reaches it.
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

// decodeCompilePlan reads the operator's declarations: identity, origins, risk
// and which steps commit external writes. Unknown fields are rejected, because
// a plan whose "writes" key is misspelled would otherwise compile every write
// in the flow as a read.
func decodeCompilePlan(data []byte) (recipe.CompileOptions, error) {
	var options recipe.CompileOptions
	if err := decodeStrict(data, &options); err != nil {
		return recipe.CompileOptions{}, fmt.Errorf("parse compile plan: %w", err)
	}
	return options, nil
}

// traceStepEnvelope is a scoped trace step plus the recorder's own bookkeeping
// fields, which the compiler ignores. It exists so the trace can be decoded
// strictly: the reason given for rejecting an unknown key in the plan applies
// harder here, because among the keys the compiler reads is `redacted`, the
// strongest of the three credential signals. A trace whose recorder spelled it
// differently would be absorbed in silence, leaving an accessible-name regex as
// the only thing between a password field and a publishable draft.
type traceStepEnvelope struct {
	recipe.TraceStep
	TabID      string `json:"tab_id,omitempty"`
	Repeat     int    `json:"repeat,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

// decodeTraceSteps accepts a bare array of scoped trace steps, or the object a
// scoped trace buffer is served as.
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

// decodeStrict refuses an unrecognised key and trailing JSON, so a shape
// mismatch is reported rather than absorbed.
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
