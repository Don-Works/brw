package main

import (
	"context"
	"errors"
	"fmt"
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

// runBench drives the fixture suite and reports what each command cost.
//
// It launches its own browser and serves the fixtures itself, so it needs no
// daemon and no network. The record is written where a later run can be
// compared against it; the summary is what a human reads.
func runBench(opts benchOptions) error {
	record, err := bench.Run(context.Background(), bench.Options{
		RepoRoot: opts.RepoRoot,
		Only:     opts.Only,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		return err
	}

	if opts.OutPath != "" {
		if err := writeBenchRecord(opts.OutPath, record); err != nil {
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

func writeBenchRecord(path string, record bench.Record) error {
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
	return record.WriteJSON(file)
}
