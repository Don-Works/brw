package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// agg accumulates trials for one (task, competitor) cell.
type agg struct {
	n           int
	passes      int
	errors      int
	wallMS      float64
	turns       float64
	browser     float64
	search      float64
	tools       float64
	outTok      float64
	cacheRead   float64
	cacheCreate float64
	obsBytes    float64
	shotBytes   float64
	cost        float64
	lastTools   map[string]int
	lastAnswer  string
}

func (a *agg) add(m RunMetrics) {
	a.n++
	if m.Pass {
		a.passes++
	}
	if m.IsError {
		a.errors++
	}
	a.wallMS += float64(m.WallMS)
	a.turns += float64(m.NumTurns)
	a.browser += float64(m.BrowserCalls)
	a.search += float64(m.ToolSearchCalls)
	a.tools += float64(m.ToolCalls)
	a.outTok += float64(m.OutputTokens)
	a.cacheRead += float64(m.CacheReadTokens)
	a.cacheCreate += float64(m.CacheCreateTok)
	a.obsBytes += float64(m.ObservationByte)
	a.shotBytes += float64(m.ScreenshotByte)
	a.cost += m.CostUSD
	a.lastTools = m.ToolsByName
	a.lastAnswer = m.Answer
}

func (a *agg) ok() bool   { return a.n > 0 && a.passes*2 >= a.n }
func (a *agg) mean(v float64) float64 {
	if a.n == 0 {
		return 0
	}
	return v / float64(a.n)
}

func renderScorecard(ladder Ladder, runs []RunMetrics, model string) string {
	// cells[taskID][competitor]
	cells := map[string]map[string]*agg{}
	order := []string{}
	seen := map[string]bool{}
	// Competitor lanes, collected in first-seen order so any subset (ours, claude,
	// ours-mcplexer) renders correctly.
	comps := []string{}
	compSeen := map[string]bool{}
	for _, m := range runs {
		if _, ok := cells[m.TaskID]; !ok {
			cells[m.TaskID] = map[string]*agg{}
		}
		if cells[m.TaskID][m.Competitor] == nil {
			cells[m.TaskID][m.Competitor] = &agg{}
		}
		cells[m.TaskID][m.Competitor].add(m)
		if !seen[m.TaskID] {
			seen[m.TaskID] = true
			order = append(order, m.TaskID)
		}
		if !compSeen[m.Competitor] {
			compSeen[m.Competitor] = true
			comps = append(comps, m.Competitor)
		}
	}

	taskName := map[string]Task{}
	for _, t := range ladder.Tasks {
		taskName[t.ID] = t
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Arena scorecard — agent-browser (ours) vs Claude-in-Chrome\n\n")
	fmt.Fprintf(&b, "Model: `%s` (both sides) · identical headless `claude -p` agent loop · cheat tools banned.\n", model)
	fmt.Fprintf(&b, "Lower is better for turns / browser-calls / search-calls / output-tokens / obs-KB / cost / wall.\n\n")

	// Per-task table.
	fmt.Fprintf(&b, "## Per-task\n\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tSURFACE\tPASS\tWALL_s\tTURNS\tBROWSER\tSEARCH\tOUT_TOK\tSEM_KB\tIMG_KB\tCOST_$")
	for _, id := range order {
		for _, c := range comps {
			a := cells[id][c]
			if a == nil {
				continue
			}
			fmt.Fprintf(tw, "T%d %s\t%s\t%s\t%.1f\t%.0f\t%.0f\t%.0f\t%.0f\t%.1f\t%.0f\t%.3f\n",
				taskName[id].Tier, id, c, passStr(a),
				a.mean(a.wallMS)/1000, a.mean(a.turns), a.mean(a.browser), a.mean(a.search),
				a.mean(a.outTok), a.mean(a.obsBytes)/1024, a.mean(a.shotBytes)/1024, a.cost/float64(maxi(a.n, 1)))
		}
	}
	tw.Flush()

	// Aggregate per competitor.
	fmt.Fprintf(&b, "\n## Totals\n\n")
	tw2 := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw2, "SURFACE\tPASSED\tTURNS_avg\tBROWSER_avg\tSEARCH_avg\tOUT_TOK_avg\tSEM_KB_avg\tIMG_KB_avg\tWALL_s_avg\tCOST_$_sum")
	for _, c := range comps {
		var tot agg
		for _, id := range order {
			if a := cells[id][c]; a != nil {
				tot.n += a.n
				tot.passes += a.passes
				tot.turns += a.turns
				tot.browser += a.browser
				tot.search += a.search
				tot.outTok += a.outTok
				tot.cacheRead += a.cacheRead
				tot.obsBytes += a.obsBytes
				tot.shotBytes += a.shotBytes
				tot.wallMS += a.wallMS
				tot.cost += a.cost
			}
		}
		fmt.Fprintf(tw2, "%s\t%d/%d\t%.0f\t%.0f\t%.0f\t%.0f\t%.1f\t%.0f\t%.1f\t%.2f\n",
			c, tot.passes, tot.n, tot.mean(tot.turns), tot.mean(tot.browser), tot.mean(tot.search),
			tot.mean(tot.outTok), tot.mean(tot.obsBytes)/1024, tot.mean(tot.shotBytes)/1024,
			tot.mean(tot.wallMS)/1000, tot.cost)
	}
	tw2.Flush()

	// Head-to-head on tasks both passed.
	fmt.Fprintf(&b, "\n## Head-to-head (tasks both surfaces passed)\n\n")
	metrics := []struct {
		name string
		get  func(*agg) float64
	}{
		{"turns", func(a *agg) float64 { return a.mean(a.turns) }},
		{"output tokens", func(a *agg) float64 { return a.mean(a.outTok) }},
		{"semantic obs KB", func(a *agg) float64 { return a.mean(a.obsBytes) / 1024 }},
		{"screenshot KB", func(a *agg) float64 { return a.mean(a.shotBytes) / 1024 }},
		{"wall seconds", func(a *agg) float64 { return a.mean(a.wallMS) / 1000 }},
		{"cost $", func(a *agg) float64 { return a.cost / float64(maxi(a.n, 1)) }},
	}
	const claudeLane = "claude-chrome"
	for _, oursLane := range comps {
		if oursLane == claudeLane {
			continue
		}
		bothPassed := []string{}
		for _, id := range order {
			o, c := cells[id][oursLane], cells[id][claudeLane]
			if o != nil && c != nil && o.ok() && c.ok() {
				bothPassed = append(bothPassed, id)
			}
		}
		fmt.Fprintf(&b, "### %s vs %s\n\n", oursLane, claudeLane)
		if len(bothPassed) == 0 {
			fmt.Fprintf(&b, "_No task passed by both; see per-task table._\n\n")
			continue
		}
		tw3 := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw3, "METRIC\t%s\tCLAUDE\tWINNER\tRATIO\n", strings.ToUpper(oursLane))
		for _, mt := range metrics {
			var os, cs float64
			for _, id := range bothPassed {
				os += mt.get(cells[id][oursLane])
				cs += mt.get(cells[id][claudeLane])
			}
			os /= float64(len(bothPassed))
			cs /= float64(len(bothPassed))
			w := "claude"
			if os < cs {
				w = oursLane
			} else if os == cs {
				w = "tie"
			}
			fmt.Fprintf(tw3, "%s\t%.2f\t%.2f\t%s\t%s\n", mt.name, os, cs, w, ratio(os, cs))
		}
		tw3.Flush()
		fmt.Fprintf(&b, "\n(%d task(s) counted: %s)\n\n", len(bothPassed), strings.Join(bothPassed, ", "))
	}

	b.WriteString(autoNotes(cells, order, comps))
	return b.String()
}

// autoNotes derives a couple of headline findings directly from the data.
func autoNotes(cells map[string]map[string]*agg, order, comps []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Notes & findings\n\n")

	var oursSearch, oursCacheRd, claudeCacheRd, oursShot, claudeShot, n float64
	for _, id := range order {
		if a := cells[id]["ours"]; a != nil {
			oursSearch += a.mean(a.search)
			oursCacheRd += a.mean(a.cacheRead)
			oursShot += a.mean(a.shotBytes)
			n++
		}
		if a := cells[id]["claude-chrome"]; a != nil {
			claudeCacheRd += a.mean(a.cacheRead)
			claudeShot += a.mean(a.shotBytes)
		}
	}
	if n > 0 {
		if oursSearch/n >= 0.5 {
			fmt.Fprintf(&b, "- **Tool-deferral tax (ours):** agent-browser's large MCP surface trips Claude Code tool-deferral, so the agent spends ~%.1f `ToolSearch` discovery call(s) per task before it can act. Claude-in-Chrome's leaner surface loads directly (0).\n", oursSearch/n)
		}
		fmt.Fprintf(&b, "- **Tool-definition context:** ours pulls ~%.0fk cached input tokens/turn vs claude ~%.0fk — our tool schemas are heavier in context.\n", oursCacheRd/n/1000, claudeCacheRd/n/1000)
		if oursShot/n > 50*1024 {
			fmt.Fprintf(&b, "- **Screenshot bloat (ours):** when our agent falls back to `browser_screenshot`, it returns a full-page base64 image averaging ~%.0fKB (vs claude ~%.0fKB) — Claude downscales viewport captures. Cap/downscale screenshot output and improve semantic coverage on dynamic pages so the agent screenshots less.\n", oursShot/n/1024, claudeShot/n/1024)
		}
	}
	fmt.Fprintf(&b, "- **Asymmetries (fairness caveats):** ours drives a *fresh isolated* Chrome (temp profile, no cookies/auth) via direct CDP; claude drives the *installed* Chrome (Profile 1, logged-in). Both headed, same model, same prompts. Auth-gated/cookie-wall differences favor claude; tool-deferral overhead penalizes ours.\n")
	fmt.Fprintf(&b, "- Token figures are from Claude Code's own `usage` accounting; cache_read/cache_creation fluctuate with prompt-cache warmth between runs — prefer turns / output-tokens / observation-bytes for stable surface comparison.\n")
	return b.String()
}

func passStr(a *agg) string {
	if a.errors > 0 && a.passes == 0 {
		return "ERR"
	}
	if a.n > 1 {
		return fmt.Sprintf("%d/%d", a.passes, a.n)
	}
	if a.ok() {
		return "yes"
	}
	return "no"
}

func winner(ours, claude float64) string {
	switch {
	case ours < claude:
		return "ours"
	case claude < ours:
		return "claude"
	default:
		return "tie"
	}
}

func ratio(ours, claude float64) string {
	if claude == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2fx", ours/claude)
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
