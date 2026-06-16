// Command arena races two browser-automation tool surfaces head-to-head on an
// identical ladder of tasks, driven by an identical headless `claude -p` agent
// loop. The only thing that differs between the two competitors is which browser
// tool surface the agent is handed:
//
//   - ours          : the agent_browser MCP (semantic refs), via a persistent
//                     agent-browserd reached through a thin --upstream-http proxy.
//   - claude-chrome : the real Claude-in-Chrome extension (`claude --chrome`).
//
// Same model, same harness, same banned "cheat" tools (no curl/WebFetch), so the
// measured deltas in tokens / round-trips / wall-clock / success are attributable
// to the browser tool surface itself. Every run's usage, cost, turn count and raw
// transcript are captured for the scorecard.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func itoa(i int) string { return strconv.Itoa(i) }

func main() {
	var (
		ladderPath string
		model      string
		trials     int
		oursMCP     string
		mcplexerMCP string
		lanes       string
		oursHealth  string
		resultsDir  string
		only        string
		tierMax     int
		timeout     time.Duration
	)
	flag.StringVar(&ladderPath, "ladder", "tests/arena/ladder.json", "task ladder JSON path")
	flag.StringVar(&model, "model", "sonnet", "model alias for both competitors (sonnet|opus|haiku|...)")
	flag.IntVar(&trials, "trials", 1, "trials per competitor per task")
	flag.StringVar(&oursMCP, "ours-mcp", "tests/arena/ours-mcp.json", "MCP config that mounts agent_browser for the ours competitor")
	flag.StringVar(&mcplexerMCP, "mcplexer-mcp", "tests/arena/mcplexer-mcp.json", "MCP config that mounts mcplexer for the ours-mcplexer (production-path) competitor")
	flag.StringVar(&lanes, "lanes", "ours,claude", "comma-separated competitors to race: ours, claude, mcplexer")
	flag.StringVar(&oursHealth, "ours-health", "http://127.0.0.1:17320/health", "health URL for the persistent ours daemon (preflight)")
	flag.StringVar(&resultsDir, "results-dir", "tests/arena/results", "directory for run transcripts + scorecard")
	flag.StringVar(&only, "only", "", "comma-separated task ids to run")
	flag.IntVar(&tierMax, "tier-max", 0, "only run tasks with tier <= this value (0 = all)")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "per-run timeout")
	flag.Parse()

	ladder, err := loadLadder(ladderPath)
	if err != nil {
		fatal(err)
	}
	oursMCPAbs, err := filepath.Abs(oursMCP)
	if err != nil {
		fatal(err)
	}

	preflight(oursHealth)

	// Isolate sub-agents from this repo's CLAUDE.md / .mcp.json by running them
	// from an empty working directory.
	workdir, err := os.MkdirTemp("", "arena-work-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(workdir)

	stamp := time.Now().Format("20060102-150405")
	outDir := filepath.Join(resultsDir, stamp)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}
	runsFile, err := os.Create(filepath.Join(outDir, "runs.jsonl"))
	if err != nil {
		fatal(err)
	}
	defer runsFile.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mcplexerMCPAbs, _ := filepath.Abs(mcplexerMCP)
	var competitors []Competitor
	for _, lane := range strings.Split(lanes, ",") {
		switch strings.TrimSpace(lane) {
		case "ours":
			competitors = append(competitors, oursCompetitor(oursMCPAbs))
		case "claude":
			competitors = append(competitors, claudeCompetitor())
		case "mcplexer":
			competitors = append(competitors, mcplexerCompetitor(mcplexerMCPAbs))
		case "":
		default:
			fatal(fmt.Errorf("unknown lane %q (want ours|claude|mcplexer)", lane))
		}
	}
	if len(competitors) == 0 {
		fatal(fmt.Errorf("no lanes selected"))
	}
	selected := parseCSV(only)

	fmt.Printf("arena: model=%s trials=%d ladder=%s\n", model, trials, ladderPath)
	fmt.Printf("results: %s\n\n", outDir)

	var runs []RunMetrics
	for _, task := range ladder.Tasks {
		if len(selected) > 0 && !selected[task.ID] {
			continue
		}
		if tierMax > 0 && task.Tier > tierMax {
			continue
		}
		fmt.Printf("=== T%d %-14s %s\n", task.Tier, task.ID, task.Name)
		for trial := 1; trial <= trials; trial++ {
			for _, comp := range competitors {
				if ctx.Err() != nil {
					fmt.Println("interrupted")
					goto done
				}
				raw := filepath.Join(outDir, fmt.Sprintf("%s.%s.t%d.jsonl", task.ID, comp.Name, trial))
				m := runOnce(ctx, comp, task, model, workdir, raw, timeout)
				m.Trial = trial
				runs = append(runs, m)
				enc, _ := json.Marshal(m)
				fmt.Fprintln(runsFile, string(enc))
				fmt.Printf("    %-13s %s turns=%-2d browser=%-2d search=%-2d out_tok=%-5d sem=%-7s img=%-7s wall=%4.1fs $%.3f  %q\n",
					comp.Name, verdict(m), m.NumTurns, m.BrowserCalls, m.ToolSearchCalls,
					m.OutputTokens, kb(m.ObservationByte), kb(m.ScreenshotByte), float64(m.WallMS)/1000, m.CostUSD, trim(m.Answer, 44))
				if m.Err != "" {
					fmt.Printf("                  error: %s\n", m.Err)
				}
			}
		}
	}
done:
	scorecard := renderScorecard(ladder, runs, model)
	fmt.Print("\n" + scorecard)
	_ = os.WriteFile(filepath.Join(outDir, "scorecard.md"), []byte(scorecard), 0o644)
	fmt.Printf("\nwrote %s\n", filepath.Join(outDir, "scorecard.md"))
}

func preflight(healthURL string) {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		fmt.Printf("WARNING: ours daemon health check failed (%s): %v\n", healthURL, err)
		fmt.Println("         the 'ours' competitor needs a persistent agent-browserd; start it first, e.g.:")
		fmt.Println("         agent-browserd --http 127.0.0.1:17320 --remote-debugging-port 0 --user-data-dir /tmp/arena-ours-chrome")
		return
	}
	resp.Body.Close()
}

func verdict(m RunMetrics) string {
	if m.IsError {
		return "ERR "
	}
	if m.Pass {
		return "PASS"
	}
	return "FAIL"
}

func kb(b int) string { return fmt.Sprintf("%.1fKB", float64(b)/1024) }

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func parseCSV(raw string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "arena: %v\n", err)
	os.Exit(1)
}
