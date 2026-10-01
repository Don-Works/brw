package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Don-Works/brw/internal/usageview"
)

func usageCommandFlags() []string {
	return []string{"--path", "--since", "--layer", "--profile", "--operation", "--limit", "--json", "--watch"}
}

func usageCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts usageview.Options
	var since string
	var asJSON bool
	var watch time.Duration
	fs.StringVar(&opts.Path, "path", "", "usage directory or ledger file")
	fs.StringVar(&since, "since", "24h", "lookback duration, RFC3339 timestamp, or all")
	fs.StringVar(&opts.Layer, "layer", "", "measurement layer")
	fs.StringVar(&opts.Profile, "profile", "", "profile filter")
	fs.StringVar(&opts.Operation, "operation", "", "operation filter")
	fs.IntVar(&opts.Limit, "limit", 20, "maximum groups (0 for all)")
	fs.BoolVar(&asJSON, "json", false, "emit JSON with counter sample counts")
	fs.DurationVar(&watch, "watch", 0, "refresh interval, e.g. 5s")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 || opts.Limit < 0 || watch < 0 || (watch > 0 && watch < time.Second) {
		fmt.Fprintln(stderr, "brw usage: unexpected arguments, negative limit, or watch interval below 1s")
		return ExitUsage
	}
	if opts.Path == "" {
		var err error
		opts.Path, err = usageview.DefaultPath()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitUsage
		}
	}
	for {
		if since != "all" {
			if duration, err := time.ParseDuration(since); err == nil && duration > 0 {
				opts.Since = time.Now().Add(-duration)
			} else if stamp, err := time.Parse(time.RFC3339, since); err == nil {
				opts.Since = stamp
			} else {
				fmt.Fprintln(stderr, "brw usage: --since requires a positive duration, RFC3339 timestamp, or all")
				return ExitUsage
			}
		}
		report, err := usageview.Read(ctx, opts)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitUsage
		}
		if asJSON {
			err = json.NewEncoder(stdout).Encode(report)
		} else {
			err = renderUsage(stdout, report)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitUsage
		}
		if watch == 0 {
			return ExitOK
		}
		timer := time.NewTimer(watch)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ExitOK
		case <-timer.C:
		}
	}
}

func renderUsage(out io.Writer, report usageview.Report) error {
	if _, err := fmt.Fprintf(out, "brw usage %s · %d records · %d groups · %d files\n", report.GeneratedAt.Format(time.RFC3339), report.MatchedRecords, report.TotalGroups, report.Files); err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LAYER/SCOPE\tOPERATION\tPROFILE/MODE\tN\tERR\tIN B\tOUT B\tEST TOK IN/OUT\tPROVIDER TOK IN/OUT\tP50 MS\tP95 MS")
	for _, g := range report.Groups {
		fmt.Fprintf(w, "%s/%s\t%s\t%s/%s\t%d\t%d\t%s\t%s\t%s/%s\t%s/%s\t%s\t%s\n", g.Layer, g.Scope, g.Operation, g.Profile, g.Mode, g.Records, g.Errors, usageCounter(g.InputBytes, g.Records), usageCounter(g.OutputBytes, g.Records), usageCounter(g.InputEstimate, g.Records), usageCounter(g.OutputEstimate, g.Records), usageCounter(g.ProviderInput, g.Records), usageCounter(g.ProviderOutput, g.Records), usageDuration(g.P50MS, g.DurationSamples, g.Records), usageDuration(g.P95MS, g.DurationSamples, g.Records))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "? = unknown; * = partial coverage. Layers overlap; never sum them as total model context.\n%s\n", strings.Join(report.Notes, "\n"))
	if err != nil {
		return err
	}
	if report.Bounded || report.InvalidRecords > 0 || report.IncompleteLines > 0 || report.ReadErrors > 0 {
		_, err = fmt.Fprintf(out, "Coverage: bounded=%t invalid=%d incomplete=%d read_errors=%d\n", report.Bounded, report.InvalidRecords, report.IncompleteLines, report.ReadErrors)
	}
	return err
}

func usageCounter(c usageview.Counter, records int) string {
	if c.Samples == 0 {
		return "?"
	}
	if c.Samples < records {
		return fmt.Sprintf("%d*", c.Total)
	}
	return fmt.Sprint(c.Total)
}

func usageDuration(value float64, samples, records int) string {
	if samples == 0 {
		return "?"
	}
	if samples < records {
		return fmt.Sprintf("%.3f*", value)
	}
	return fmt.Sprintf("%.3f", value)
}
