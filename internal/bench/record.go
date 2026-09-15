// Package bench measures what one brw command costs against the local fixture
// suite: wall time, CDP round trips, bytes over the transport, and the token
// size of the MCP tool result the agent gets back, plus the machine cost of the
// whole run.
//
// It is a measurement, not a gate. Nothing here runs under `go test ./...`;
// timings taken on a loaded CI machine would fail for reasons that have nothing
// to do with the change under test.
package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/mcp"
)

// RecordSchema names the shape of the machine-readable record. A consumer that
// does not recognise it should refuse to compare rather than guess.
//
// v2 because the observation columns changed meaning: v1 weighed the internal
// Go result once, v2 weighs the MCP tool result an agent is actually sent. The
// two are the same measurement by name and roughly a factor of two apart, which
// is exactly the comparison the schema field exists to stop.
const RecordSchema = "brw.bench/v2"

// charsPerToken is the same rough estimator scripts/measure-tool-catalogue.py
// uses, kept identical so the two sets of published numbers are on one scale.
// It compares arms; it is not a tokenizer.
const charsPerToken = 4

// Record is one complete run.
type Record struct {
	Schema      string                `json:"schema"`
	StartedAt   time.Time             `json:"started_at"`
	DurationMS  int64                 `json:"duration_ms"`
	Environment harness.Environment   `json:"environment"`
	System      harness.ResourceUsage `json:"system"`
	BrowserCost harness.Usage         `json:"browser_cost"`
	HarnessCost harness.Usage         `json:"harness_cost"`
	Flows       []Flow                `json:"flows"`
	Totals      Totals                `json:"totals"`
	OK          bool                  `json:"ok"`
	Notes       map[string]any        `json:"notes,omitempty"`
}

// Flow is one fixture driven end to end.
type Flow struct {
	ID       string    `json:"id"`
	Fixture  string    `json:"fixture"`
	URL      string    `json:"url"`
	Commands []Command `json:"commands"`
	Totals   Totals    `json:"totals"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
}

// Command is one measured call.
//
// CDPCommands counts the messages brw sent the browser and CDPMessages what
// came back — responses and events together. The counters are sampled around
// each call, so an event that arrives while no call is in flight is attributed
// to the next command rather than to the one that caused it.
//
// ObservationBytes is the MCP tool result, envelope included, not the internal
// Go value: see ObservationBytes.
type Command struct {
	Name              string  `json:"name"`
	Tool              string  `json:"tool"`
	WallMS            float64 `json:"wall_ms"`
	CDPCommands       int64   `json:"cdp_commands"`
	CDPMessages       int64   `json:"cdp_messages"`
	TransportBytesTx  int64   `json:"transport_bytes_tx"`
	TransportBytesRx  int64   `json:"transport_bytes_rx"`
	ObservationBytes  int     `json:"observation_bytes"`
	ObservationTokens int     `json:"observation_tokens"`
	OK                bool    `json:"ok"`
	Error             string  `json:"error,omitempty"`
}

// Totals is the sum over a flow or a run.
type Totals struct {
	Commands          int     `json:"commands"`
	WallMS            float64 `json:"wall_ms"`
	CDPCommands       int64   `json:"cdp_commands"`
	CDPMessages       int64   `json:"cdp_messages"`
	TransportBytesTx  int64   `json:"transport_bytes_tx"`
	TransportBytesRx  int64   `json:"transport_bytes_rx"`
	ObservationBytes  int     `json:"observation_bytes"`
	ObservationTokens int     `json:"observation_tokens"`
}

// AddCommand folds one measurement into a running total.
func (t *Totals) AddCommand(cmd Command) {
	t.Commands++
	t.WallMS += cmd.WallMS
	t.CDPCommands += cmd.CDPCommands
	t.CDPMessages += cmd.CDPMessages
	t.TransportBytesTx += cmd.TransportBytesTx
	t.TransportBytesRx += cmd.TransportBytesRx
	t.ObservationBytes += cmd.ObservationBytes
	t.ObservationTokens += cmd.ObservationTokens
}

// AddTotals folds a flow's totals into the run's.
func (t *Totals) AddTotals(other Totals) {
	t.Commands += other.Commands
	t.WallMS += other.WallMS
	t.CDPCommands += other.CDPCommands
	t.CDPMessages += other.CDPMessages
	t.TransportBytesTx += other.TransportBytesTx
	t.TransportBytesRx += other.TransportBytesRx
	t.ObservationBytes += other.ObservationBytes
	t.ObservationTokens += other.ObservationTokens
}

// EstimateTokens converts an observation size to the rough token count an agent
// pays for it.
func EstimateTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return bytes / charsPerToken
}

// ObservationBytes is the size of the MCP tool result an agent receives for a
// value, measured through the same payload builder the server serializes.
//
// It is not the size of the internal Go result. MCP sends the payload twice —
// once as content[0].text, a JSON string with every quote escaped, and again as
// structuredContent — so weighing the Go value alone reports roughly half of
// what the turn costs, under a column heading that says otherwise.
//
// A value that cannot be marshalled is reported as zero-sized rather than
// failing the measurement, because the command itself still ran.
func ObservationBytes(value any) int {
	if value == nil {
		return 0
	}
	payload, err := mcp.ToolResultPayload(value)
	if err != nil {
		return 0
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(data)
}

// WriteJSON emits the machine-readable record.
func (r Record) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// WriteSummary emits the human form: the fingerprint first, because a table of
// milliseconds means nothing without it.
func (r Record) WriteSummary(w io.Writer) {
	fmt.Fprintf(w, "brw benchmark %s\n", r.Schema)
	fmt.Fprintf(w, "environment: %s\n", r.Environment.Fingerprint())
	fmt.Fprintf(w, "captured:    %s\n", r.Environment.CapturedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "run:         %d commands in %d ms wall\n\n", r.Totals.Commands, r.DurationMS)

	for _, flow := range r.Flows {
		fmt.Fprintf(w, "%s (%s)\n", flow.ID, flow.Fixture)
		table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "  COMMAND\tTOOL\tMS\tCDP TX\tCDP RX\tBYTES TX\tBYTES RX\tOBS B\tOBS TOK\tOK")
		for _, cmd := range flow.Commands {
			_, _ = fmt.Fprintf(table, "  %s\t%s\t%.1f\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
				cmd.Name, cmd.Tool, cmd.WallMS, cmd.CDPCommands, cmd.CDPMessages,
				cmd.TransportBytesTx, cmd.TransportBytesRx,
				cmd.ObservationBytes, cmd.ObservationTokens, yesNo(cmd.OK))
		}
		writeTotalsRow(table, "  TOTAL", flow.Totals)
		_ = table.Flush()
		if flow.Error != "" {
			fmt.Fprintf(w, "  failed: %s\n", flow.Error)
		}
		fmt.Fprintln(w)
	}

	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "RUN\tTOOL\tMS\tCDP TX\tCDP RX\tBYTES TX\tBYTES RX\tOBS B\tOBS TOK\tOK")
	writeTotalsRow(table, "all flows", r.Totals)
	_ = table.Flush()

	fmt.Fprintln(w)
	if r.System.Supported {
		fmt.Fprintf(w, "harness process: %s\n", r.HarnessCost.Describe())
		fmt.Fprintf(w, "browser tree:    %s\n", r.BrowserCost.Describe())
		fmt.Fprintln(w, "browser peak RSS is the largest single browser process, which is what the kernel records.")
	} else {
		fmt.Fprintln(w, "system metrics: unavailable on this platform")
	}
	if !r.OK {
		fmt.Fprintln(w, "\nRUN FAILED — the numbers above describe a run that did not complete")
	}
}

func writeTotalsRow(w io.Writer, label string, totals Totals) {
	_, _ = fmt.Fprintf(w, "%s\t%d cmds\t%.1f\t%d\t%d\t%d\t%d\t%d\t%d\t\n",
		label, totals.Commands, totals.WallMS, totals.CDPCommands, totals.CDPMessages,
		totals.TransportBytesTx, totals.TransportBytesRx,
		totals.ObservationBytes, totals.ObservationTokens)
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}
