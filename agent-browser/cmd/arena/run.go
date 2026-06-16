package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Competitor is one browser-automation surface under test. Both are driven by
// an identical headless `claude -p` agent loop; only ExtraArgs differ (which
// browser tool surface the agent is given).
type Competitor struct {
	Name      string   // "ours" | "claude-chrome"
	ExtraArgs []string // surface-specific claude flags
}

// cheatTools are disabled on BOTH sides so the only way to touch the web is the
// browser tool surface under test (no curl/WebFetch shortcuts).
const cheatTools = "WebFetch,WebSearch,Bash,Read,Write,Edit,Glob,Grep,NotebookEdit,TodoWrite,Task"

func oursCompetitor(mcpConfigPath string) Competitor {
	return Competitor{
		Name:      "ours",
		ExtraArgs: []string{"--no-chrome", "--strict-mcp-config", "--mcp-config", mcpConfigPath},
	}
}

func claudeCompetitor() Competitor {
	return Competitor{
		Name:      "claude-chrome",
		ExtraArgs: []string{"--chrome", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`},
	}
}

// mcplexerCompetitor drives agent_browser through mcplexer's slim 4-tool surface
// (mcpx__execute_code + mcpx__search_tools) instead of mounting agent_browser as a
// raw MCP server. This is the production path: Claude Code never sees the full tool
// list, so its third-party-MCP tool-deferral never fires (no ToolSearch tax), and
// the agent batches several browser calls in one execute_code round-trip. The
// append-system-prompt makes a sonnet agent reliably reach for it.
func mcplexerCompetitor(mcpConfigPath string) Competitor {
	const steer = "To drive the browser, use mcplexer: call mcpx__execute_code with JS that invokes agent_browser tools directly, e.g. agent_browser.browser_open({url}), agent_browser.browser_snapshot({}), agent_browser.browser_find({role,query}), agent_browser.browser_click({ref}), browser_type, browser_fill, browser_hover, browser_drag, browser_press, browser_select. Batch related calls in ONE execute_code snippet and print() what you need. Discover signatures with mcpx__search_tools if unsure."
	return Competitor{
		Name:      "ours-mcplexer",
		ExtraArgs: []string{"--no-chrome", "--strict-mcp-config", "--mcp-config", mcpConfigPath, "--append-system-prompt", steer},
	}
}

// RunMetrics is one (competitor, task, trial) measurement.
type RunMetrics struct {
	Competitor      string         `json:"competitor"`
	TaskID          string         `json:"task_id"`
	Tier            int            `json:"tier"`
	Trial           int            `json:"trial"`
	Pass            bool           `json:"pass"`
	IsError         bool           `json:"is_error"`
	WallMS          int64          `json:"wall_ms"`
	DurationMS      int64          `json:"duration_ms"`
	APIDurationMS   int64          `json:"api_duration_ms"`
	NumTurns        int            `json:"num_turns"`
	CostUSD         float64        `json:"cost_usd"`
	InputTokens     int            `json:"input_tokens"`
	OutputTokens    int            `json:"output_tokens"`
	CacheReadTokens int            `json:"cache_read_tokens"`
	CacheCreateTok  int            `json:"cache_create_tokens"`
	ToolCalls       int            `json:"tool_calls"`
	ToolSearchCalls int            `json:"toolsearch_calls"`
	BrowserCalls    int            `json:"browser_calls"`
	ObservationByte int            `json:"observation_bytes"` // semantic (text) tool-result bytes
	ScreenshotByte  int            `json:"screenshot_bytes"`  // base64 image bytes in tool results
	ToolsByName     map[string]int `json:"tools_by_name"`
	Answer          string         `json:"answer"`
	Err             string         `json:"error,omitempty"`
}

// streamEvent is the subset of a `claude --output-format stream-json` line we read.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// result event
	IsError       bool    `json:"is_error"`
	DurationMS    int64   `json:"duration_ms"`
	DurationAPIMS int64   `json:"duration_api_ms"`
	NumTurns      int     `json:"num_turns"`
	CostUSD       float64 `json:"total_cost_usd"`
	Result        string  `json:"result"`
	Usage         *struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheReadInputToks  int `json:"cache_read_input_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	// assistant / user events
	Message *struct {
		Content []struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

// runOnce executes a single competitor/task trial and returns its metrics. The
// raw stream-json transcript is written to rawPath for evidence/debugging.
func runOnce(ctx context.Context, comp Competitor, task Task, model, workdir, rawPath string, timeout time.Duration) RunMetrics {
	m := RunMetrics{
		Competitor:  comp.Name,
		TaskID:      task.ID,
		Tier:        task.Tier,
		ToolsByName: map[string]int{},
	}

	args := []string{
		"-p", task.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--dangerously-skip-permissions",
		"--disallowedTools", cheatTools,
	}
	args = append(args, comp.ExtraArgs...)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "claude", args...)
	cmd.Dir = workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	m.WallMS = time.Since(start).Milliseconds()

	if rawPath != "" {
		_ = os.WriteFile(rawPath, stdout.Bytes(), 0o644)
	}

	parseStream(stdout.Bytes(), &m)

	if runErr != nil && m.NumTurns == 0 {
		m.IsError = true
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		m.Err = msg
	}

	m.Pass = !m.IsError && task.pass(m.Answer)
	return m
}

func parseStream(out []byte, m *RunMetrics) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			for _, c := range ev.Message.Content {
				if c.Type != "tool_use" {
					continue
				}
				m.ToolCalls++
				m.ToolsByName[c.Name]++
				switch {
				case c.Name == "ToolSearch":
					m.ToolSearchCalls++
				case strings.HasPrefix(c.Name, "mcp__"):
					m.BrowserCalls++
				}
			}
		case "user":
			if ev.Message == nil {
				continue
			}
			for _, c := range ev.Message.Content {
				if c.Type == "tool_result" {
					sem, img := measureToolResult(c.Content)
					m.ObservationByte += sem
					m.ScreenshotByte += img
				}
			}
		case "result":
			m.IsError = ev.IsError
			m.DurationMS = ev.DurationMS
			m.APIDurationMS = ev.DurationAPIMS
			m.NumTurns = ev.NumTurns
			m.CostUSD = ev.CostUSD
			m.Answer = strings.TrimSpace(ev.Result)
			if ev.Usage != nil {
				m.InputTokens = ev.Usage.InputTokens
				m.OutputTokens = ev.Usage.OutputTokens
				m.CacheReadTokens = ev.Usage.CacheReadInputToks
				m.CacheCreateTok = ev.Usage.CacheCreationTokens
			}
		}
	}
}

// measureToolResult splits a tool_result's content into semantic (text) bytes
// and image (base64 screenshot) bytes. A result is either a JSON string, or an
// array of content blocks ({type:text|image|tool_reference,...}). Screenshots
// returned as base64 images otherwise swamp the "observation bytes" signal, so
// we attribute them separately and keep the semantic comparison honest.
func measureToolResult(raw json.RawMessage) (semantic, image int) {
	if len(raw) == 0 {
		return 0, 0
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return len(s), 0
		}
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ToolName string `json:"tool_name"`
		Source   *struct {
			Data string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "image" {
				if b.Source != nil {
					image += len(b.Source.Data)
				}
				continue
			}
			semantic += len(b.Text) + len(b.ToolName)
		}
		return semantic, image
	}
	return len(raw), 0
}

// topTools renders the tool-call histogram as a compact "name×n, name×n" string.
func topTools(byName map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var items []kv
	for k, v := range byName {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	var parts []string
	for _, it := range items {
		name := strings.TrimPrefix(it.k, "mcp__")
		parts = append(parts, name+"×"+itoa(it.v))
	}
	return strings.Join(parts, ", ")
}
