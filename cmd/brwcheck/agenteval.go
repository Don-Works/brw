package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Don-Works/brw/internal/agenteval"
)

type evalOptions struct {
	RepoRoot string
	Only     string
	JSON     bool
	OutPath  string
	// Verify runs every task honestly AND sabotaged, and requires the honest run
	// to pass and the sabotaged one to fail. It is how the harness is shown to
	// be capable of reporting a failure.
	Verify bool
	// Judge turns on the optional LLM layer. It needs ANTHROPIC_API_KEY; without
	// one the run is graded by the deterministic end-state check alone.
	Judge bool
}

// evalBudget and verifyBudget bound the whole run, the way benchBudget does.
// Verify runs every task twice and may wait on the judge, so it gets more.
const (
	evalBudget   = 15 * time.Minute
	verifyBudget = 30 * time.Minute
)

// runAgentEval drives the agent-level evaluations against the local fixture
// suite. Like the benchmark it launches its own browser and serves the fixtures
// itself, so it needs no daemon; unlike the benchmark it needs no network
// either unless the judge is asked for.
func runAgentEval(opts evalOptions) error {
	modes := []agenteval.Mode{agenteval.ModeHonest}
	budget := evalBudget
	if opts.Verify {
		modes = agenteval.Modes()
		budget = verifyBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var judge *agenteval.Judge
	if opts.Judge {
		resolved, ok := agenteval.NewJudgeFromEnv()
		if !ok {
			return errors.New("--eval-judge needs ANTHROPIC_API_KEY; without it the run is graded by the end-state check alone")
		}
		judge = resolved
	}

	report, err := agenteval.Run(ctx, agenteval.Options{
		RepoRoot: opts.RepoRoot,
		Only:     opts.Only,
		Modes:    modes,
		Judge:    judge,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("evaluation run exceeded its %s budget: %w", budget, err)
		}
		return err
	}

	if opts.OutPath != "" {
		if err := writeEvalReport(opts.OutPath, report); err != nil {
			return err
		}
	}
	if opts.JSON {
		if err := report.WriteJSON(os.Stdout); err != nil {
			return err
		}
	} else {
		report.WriteSummary(os.Stdout)
	}
	if opts.OutPath != "" && !opts.JSON {
		fmt.Printf("\nreport written to %s\n", opts.OutPath)
	}
	if !report.OK {
		return errors.New("agent evaluation failed")
	}
	return nil
}

func writeEvalReport(path string, report agenteval.Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return report.WriteJSON(file)
}
