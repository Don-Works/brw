package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Don-Works/brw/internal/bench"
)

type benchOptions struct {
	RepoRoot string
	Only     string
	JSON     bool
	OutPath  string
}

const benchBudget = 10 * time.Minute

func runBench(opts benchOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), benchBudget)
	defer cancel()

	record, err := bench.Run(ctx, bench.Options{
		RepoRoot: opts.RepoRoot,
		Only:     opts.Only,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("benchmark run exceeded its %s budget: %w", benchBudget, err)
		}
		return err
	}

	if opts.OutPath != "" {
		if err := writeJSONReport(opts.OutPath, record.WriteJSON); err != nil {
			return err
		}
	}
	if opts.JSON {
		if err := record.WriteJSON(os.Stdout); err != nil {
			return err
		}
	} else {
		record.WriteSummary(os.Stdout)
	}
	if opts.OutPath != "" && !opts.JSON {
		fmt.Printf("\nrecord written to %s\n", opts.OutPath)
	}
	if !record.OK {
		return errors.New("benchmark run failed")
	}
	return nil
}

func writeJSONReport(path string, write func(io.Writer) error) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	return errors.Join(write(file), file.Close())
}
