package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type benchOptions struct {
	RepoRoot string
	Fixture  string
	JSON     bool
}

type benchMeasure struct {
	Action string `json:"action"`
	MS     int64  `json:"ms"`
	Bytes  int    `json:"bytes"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type benchScorecard struct {
	Fixture    string         `json:"fixture"`
	Path       string         `json:"path"`
	Transport  string         `json:"transport"`
	Rows       []benchMeasure `json:"rows"`
	TotalMS    int64          `json:"total_ms"`
	TotalBytes int            `json:"total_bytes"`
	OK         bool           `json:"ok"`
}

func runBench(opts benchOptions) error {
	root, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return err
	}
	fixture := strings.TrimSpace(opts.Fixture)
	if fixture == "" {
		fixture = "custom-combobox.html"
	}
	fixturePath := filepath.Join(root, "tests", "fixtures", fixture)
	if _, err := os.Stat(fixturePath); err != nil {
		return fmt.Errorf("fixture %s: %w", fixturePath, err)
	}

	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("", "browsercheck-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := browser.New(ctx, browser.Config{
		UserDataDir: tmpDir,
		Timeout:     20 * time.Second,
		ChromeArgs: []string{
			"--headless=new",
			"--no-sandbox",
			"--disable-gpu",
			"--disable-dev-shm-usage",
			"--hide-scrollbars",
		},
	})
	if err != nil {
		return fmt.Errorf("launch Chrome for bench: %w", err)
	}
	defer mgr.Close()

	card, err := benchComboboxFlow(ctx, mgr, fileURL(fixturePath), fixture)
	if err != nil {
		card.OK = false
	}
	if opts.JSON {
		data, encErr := json.MarshalIndent(card, "", "  ")
		if encErr != nil {
			return encErr
		}
		fmt.Println(string(data))
	} else {
		printBenchTable(os.Stdout, card)
	}
	if !card.OK {
		return err
	}
	return nil
}

func benchComboboxFlow(ctx context.Context, mgr *browser.Manager, fixtureURL, fixture string) (benchScorecard, error) {
	refs := map[string]string{}
	card := benchScorecard{
		Fixture:   fixture,
		Path:      fixtureURL,
		Transport: "direct-cdp",
	}

	run := func(name string, fn func() (any, error)) error {
		start := time.Now()
		payload, stepErr := fn()
		row := benchMeasure{
			Action: name,
			MS:     time.Since(start).Milliseconds(),
			Bytes:  payloadBytes(payload),
			OK:     stepErr == nil,
		}
		if stepErr != nil {
			row.Error = stepErr.Error()
		}
		card.Rows = append(card.Rows, row)
		card.TotalMS += row.MS
		card.TotalBytes += row.Bytes
		return stepErr
	}

	steps := []struct {
		name string
		fn   func() (any, error)
	}{
		{"open", func() (any, error) { return mgr.Open(ctx, fixtureURL) }},
		{"wait_for_load", func() (any, error) {
			err := mgr.WaitFor(ctx, "title:Custom ARIA Combobox Fixture", 5*time.Second)
			return map[string]string{"condition": "title:Custom ARIA Combobox Fixture"}, err
		}},
		{"snapshot", func() (any, error) {
			snap, err := mgr.Snapshot(ctx, snapshot.SnapshotOptions{})
			if err != nil {
				return nil, err
			}
			for _, match := range []elementMatch{
				{Role: "combobox", NameContains: "Release channel", SaveAs: "channel"},
				{Role: "button", NameContains: "Continue with channel", SaveAs: "continue"},
			} {
				el, ok := findElement(snap.Elements, match)
				if !ok {
					return snap, fmt.Errorf("missing element %s", describeMatch(match))
				}
				refs[match.SaveAs] = el.Ref
			}
			return snap, nil
		}},
		{"find", func() (any, error) {
			return mgr.Find(ctx, snapshot.FindOptions{
				Role:  "combobox",
				Text:  "Release channel",
				Limit: 5,
			})
		}},
		{"read", func() (any, error) { return mgr.Read(ctx) }},
		{"click_continue_blocked", func() (any, error) {
			return mgr.Click(ctx, refs["continue"])
		}},
		{"click_combobox", func() (any, error) {
			return mgr.Click(ctx, refs["channel"])
		}},
		{"select_option", func() (any, error) {
			return mgr.Select(ctx, refs["channel"], "Stable channel")
		}},
		{"click_continue", func() (any, error) {
			return mgr.Click(ctx, refs["continue"])
		}},
		{"assert_result", func() (any, error) {
			err := mgr.WaitFor(ctx, "text:Result: committed channel stable", 5*time.Second)
			return map[string]string{"condition": "text:Result: committed channel stable"}, err
		}},
	}

	var flowErr error
	for _, step := range steps {
		if err := run(step.name, step.fn); err != nil && flowErr == nil {
			flowErr = err
		}
	}
	card.OK = flowErr == nil
	return card, flowErr
}

func payloadBytes(v any) int {
	if v == nil {
		return 0
	}
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

func printBenchTable(out io.Writer, card benchScorecard) {
	fmt.Fprintf(out, "browsercheck bench (%s via %s)\n", card.Fixture, card.Transport)
	fmt.Fprintf(out, "fixture: %s\n\n", card.Path)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACTION\tMS\tBYTES\tOK")
	for _, row := range card.Rows {
		status := "yes"
		if !row.OK {
			status = "no"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", row.Action, row.MS, row.Bytes, status)
	}
	_, _ = fmt.Fprintf(w, "TOTAL\t%d\t%d\t\n", card.TotalMS, card.TotalBytes)
	_ = w.Flush()

	if !card.OK {
		for _, row := range card.Rows {
			if row.Error != "" {
				fmt.Fprintf(out, "\nfailed at %s: %s\n", row.Action, row.Error)
				break
			}
		}
	}
}
