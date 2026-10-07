package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Don-Works/brw/internal/agenteval"
)

type evalOptions struct {
	RepoRoot string
	Only     string
	JSON     bool
	OutPath  string
	// Verify runs every task honestly AND sabotaged, and requires the honest run to pass and the sabotaged one to fail.
	Verify bool
	// Judge turns on the optional LLM layer.
	Judge bool
}

const (
	evalBudget   = 15 * time.Minute
	verifyBudget = 30 * time.Minute
)

func evalPlanFor(verify bool) ([]agenteval.Mode, time.Duration) {
	if verify {
		return agenteval.Modes(), verifyBudget
	}
	return []agenteval.Mode{agenteval.ModeHonest}, evalBudget
}

var runEvalSuite = agenteval.Run

func runAgentEval(opts evalOptions) error {
	modes, budget := evalPlanFor(opts.Verify)
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

	report, err := runEvalSuite(ctx, agenteval.Options{
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
		if err := writeJSONReport(opts.OutPath, report.WriteJSON); err != nil {
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
