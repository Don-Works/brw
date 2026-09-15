package bench

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Options configures a run.
type Options struct {
	RepoRoot   string
	ChromePath string
	// Only restricts the run to one flow id. Empty runs the whole suite.
	Only string
	// Timeout bounds a single browser operation.
	Timeout time.Duration
}

// FlowIDs lists the flows a run drives, in order.
func FlowIDs() []string {
	ids := make([]string, 0, len(flowDefs))
	for _, def := range flowDefs {
		ids = append(ids, def.id)
	}
	return ids
}

// warmupFixture is the page the discarded warm-up flow opens. It is named here
// rather than inline so the fixture audit can include it: it is loaded by every
// run, and a warm-up that reached the network would still distort the first
// measured flow.
const warmupFixture = "content.html"

type flowDef struct {
	id      string
	fixture string
	build   func(*flowRunner) []commandDef
}

type commandDef struct {
	name string
	tool string
	run  func() (any, error)
}

// flowDefs is the suite. Each flow drives one fixture through the shape of work
// an agent actually does on it — locate, act, confirm — rather than timing a
// verb in isolation, because the cost of a snapshot is only meaningful next to
// what the actions after it then cost.
var flowDefs = []flowDef{
	{id: "forms", fixture: "forms.html", build: (*flowRunner).formsCommands},
	{id: "shop", fixture: "decathlon-shop.html", build: (*flowRunner).shopCommands},
	{id: "dynamic", fixture: "dynamic.html", build: (*flowRunner).dynamicCommands},
	{id: "structured", fixture: "structured-product.html", build: (*flowRunner).structuredCommands},
}

// Run drives the fixture suite and returns the record.
//
// A failed flow does not abort the run: the remaining flows still produce
// numbers, and the record says which flow failed and where. What it must never
// do is report OK.
func Run(ctx context.Context, opts Options) (Record, error) {
	root, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return Record{}, err
	}
	selected, err := selectFlows(opts.Only)
	if err != nil {
		return Record{}, err
	}

	fixtures, err := harness.ServeFixtures(root)
	if err != nil {
		return Record{}, err
	}
	defer fixtures.Close()
	if err := fixtures.Reachable(selected[0].fixture); err != nil {
		return Record{}, fmt.Errorf("fixture origin: %w", err)
	}
	digest, err := fixtures.Digest()
	if err != nil {
		return Record{}, err
	}

	before := harness.ReadResourceUsage()
	started := time.Now()

	rig, err := harness.LaunchBrowser(ctx, harness.BrowserOptions{
		ChromePath: opts.ChromePath,
		Timeout:    opts.Timeout,
		Meter:      true,
	})
	if err != nil {
		return Record{}, err
	}

	environment := harness.DescribeEnvironment()
	environment.Browser = rig.Version.Browser
	environment.CDPProtocol = rig.Version.Protocol
	environment.Headless = true
	environment.FixtureDigest = digest

	record := Record{
		Schema:      RecordSchema,
		StartedAt:   started.UTC(),
		Environment: environment,
		OK:          true,
		Notes: map[string]any{
			// Named from the identity constant, not a literal: the bench launches its
			// own throwaway Chrome so the lane is not in doubt, and a rename should
			// not leave the record claiming a transport that no longer exists.
			"transport":         brwidentity.TransportDirectCDP,
			"fixture_origin":    "loopback http",
			"token_estimator":   fmt.Sprintf("%d chars per token", charsPerToken),
			"observation_scope": "the mcp tool result: the payload escaped inside content[0].text plus structuredContent",
			"warmup_discarded":  true,
		},
	}

	// One discarded warm-up flow. The first page of a cold browser pays for
	// renderer startup, font loading and the first script compile, and folding
	// that into whichever flow happens to run first makes the suite's own order
	// part of the result.
	warmup := &flowRunner{ctx: ctx, rig: rig, url: fixtures.URL(warmupFixture)}
	warmupErr := warmup.warm()

	for _, def := range selected {
		flow := runFlow(ctx, rig, def, fixtures.URL(def.fixture))
		record.Flows = append(record.Flows, flow)
		record.Totals.AddTotals(flow.Totals)
		if !flow.OK {
			record.OK = false
		}
	}
	if warmupErr != nil {
		record.Notes["warmup_error"] = warmupErr.Error()
	}

	record.DurationMS = time.Since(started).Milliseconds()

	// Close the browser BEFORE the final reading: the kernel only accounts for a
	// child once it has exited and been reaped, so a reading taken with Chrome
	// still running would report the browser as free.
	if err := rig.Close(); err != nil {
		record.Notes["shutdown_error"] = err.Error()
	}
	after := harness.ReadResourceUsage()
	record.System = after
	if after.Supported && before.Supported {
		record.HarnessCost = after.Self.CPUDelta(before.Self)
		record.BrowserCost = after.Children.CPUDelta(before.Children)
	}
	return record, nil
}

func selectFlows(only string) ([]flowDef, error) {
	only = strings.TrimSpace(only)
	if only == "" {
		return flowDefs, nil
	}
	for _, def := range flowDefs {
		if def.id == only {
			return []flowDef{def}, nil
		}
	}
	return nil, fmt.Errorf("unknown flow %q; have %s", only, strings.Join(FlowIDs(), ", "))
}

func runFlow(ctx context.Context, rig *harness.Browser, def flowDef, url string) Flow {
	runner := &flowRunner{ctx: ctx, rig: rig, url: url, refs: map[string]string{}}
	flow := Flow{ID: def.id, Fixture: def.fixture, URL: url, OK: true}

	for _, cmd := range def.build(runner) {
		measured := runner.measure(cmd)
		flow.Commands = append(flow.Commands, measured)
		flow.Totals.AddCommand(measured)
		if !measured.OK {
			flow.OK = false
			flow.Error = fmt.Sprintf("%s: %s", measured.Name, measured.Error)
			break
		}
	}
	runner.closeTab()
	return flow
}

// flowRunner holds the state a flow's commands share: the tab they run against
// and the refs an earlier snapshot resolved.
type flowRunner struct {
	ctx   context.Context
	rig   *harness.Browser
	url   string
	refs  map[string]string
	tabID string
}

func (f *flowRunner) manager() *browser.Manager { return f.rig.Manager }

// tabContext pins every command in a flow to the tab the flow opened, so a
// stray tab left by something else cannot silently become the thing measured.
func (f *flowRunner) tabContext() context.Context {
	if f.tabID == "" {
		return f.ctx
	}
	return browser.WithTabID(f.ctx, f.tabID)
}

func (f *flowRunner) measure(cmd commandDef) Command {
	meter := f.rig.Meter
	var before harness.Counters
	if meter != nil {
		before = meter.Read()
	}
	start := time.Now()
	payload, err := cmd.run()
	elapsed := time.Since(start)

	result := Command{
		Name:   cmd.name,
		Tool:   cmd.tool,
		WallMS: float64(elapsed.Microseconds()) / 1000,
		OK:     err == nil,
	}
	if meter != nil {
		delta := meter.Read().Sub(before)
		result.CDPCommands = delta.CDPCommands
		result.CDPMessages = delta.CDPMessages
		result.TransportBytesTx = delta.TransportBytesTx
		result.TransportBytesRx = delta.TransportBytesRx
	}
	result.ObservationBytes = ObservationBytes(payload)
	result.ObservationTokens = EstimateTokens(result.ObservationBytes)
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// warm drives a page the suite does not measure, so the first measured command
// meets a browser that has already started a renderer.
func (f *flowRunner) warm() error {
	result, err := f.manager().Open(f.ctx, f.url)
	if err != nil {
		return err
	}
	f.tabID = result.Tab.ID
	if _, err := f.manager().Snapshot(f.tabContext(), snapshot.SnapshotOptions{}); err != nil {
		return err
	}
	f.closeTab()
	return nil
}

func (f *flowRunner) closeTab() {
	if f.tabID == "" {
		return
	}
	_ = f.manager().CloseTab(f.ctx, f.tabID)
	f.tabID = ""
}

func (f *flowRunner) open() (any, error) {
	result, err := f.manager().Open(f.ctx, f.url)
	if err != nil {
		return nil, err
	}
	f.tabID = result.Tab.ID
	return result, nil
}

// captureRefs records the refs a flow's later commands act through, and fails
// the command that took the snapshot when the fixture does not offer one. A
// missing control has to fail here, where the record names it, rather than four
// commands later as an unexplained click failure.
func (f *flowRunner) captureRefs(elements []snapshot.Element, wanted map[string]harness.ElementQuery) error {
	refs, err := harness.ResolveRefs(elements, wanted)
	if err != nil {
		return err
	}
	for key, ref := range refs {
		f.refs[key] = ref
	}
	return nil
}

func (f *flowRunner) ref(key string) (string, error) {
	ref, ok := f.refs[key]
	if !ok || ref == "" {
		return "", fmt.Errorf("no ref captured for %s", key)
	}
	return ref, nil
}

// findRef resolves one element live and remembers it, for controls a page only
// renders after an earlier action.
func (f *flowRunner) findRef(key string, opts snapshot.FindOptions) (any, error) {
	result, err := f.manager().Find(f.tabContext(), opts)
	if err != nil {
		return nil, err
	}
	if len(result.Elements) == 0 {
		return result, fmt.Errorf("find returned nothing for %s", key)
	}
	f.refs[key] = result.Elements[0].Ref
	return result, nil
}

func (f *flowRunner) formsCommands() []commandDef {
	mgr := f.manager()
	return []commandDef{
		{name: "open", tool: "brw_open", run: f.open},
		{name: "snapshot", tool: "brw_snapshot", run: func() (any, error) {
			page, err := mgr.Snapshot(f.tabContext(), snapshot.SnapshotOptions{})
			if err != nil {
				return nil, err
			}
			return page, f.captureRefs(page.Elements, map[string]harness.ElementQuery{
				"email": {Role: "textbox", Name: "Email"},
				"name":  {Role: "textbox", Name: "Full name"},
				"plan":  {Role: "combobox", Name: "Plan"},
				"terms": {Role: "checkbox", Name: "Accept terms"},
				"notes": {Role: "textbox", Name: "Project notes"},
			})
		}},
		{name: "find", tool: "brw_find", run: func() (any, error) {
			return mgr.Find(f.tabContext(), snapshot.FindOptions{Role: "textbox", Limit: 10})
		}},
		{name: "fill_email", tool: "brw_fill", run: func() (any, error) {
			return f.fill("email", "harness@example.test")
		}},
		{name: "fill_name", tool: "brw_fill", run: func() (any, error) {
			return f.fill("name", "Fixture Runner")
		}},
		{name: "select_plan", tool: "brw_select", run: func() (any, error) {
			ref, err := f.ref("plan")
			if err != nil {
				return nil, err
			}
			return mgr.Select(f.tabContext(), ref, "Pro")
		}},
		{name: "click_terms", tool: "brw_click", run: func() (any, error) {
			ref, err := f.ref("terms")
			if err != nil {
				return nil, err
			}
			return mgr.Click(f.tabContext(), ref)
		}},
		{name: "fill_notes", tool: "brw_fill", run: func() (any, error) {
			return f.fill("notes", "benchmark run")
		}},
		{name: "click_submit", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "Submit request", Role: "button"})
		}},
		{name: "wait_submitted", tool: "brw_wait_for", run: func() (any, error) {
			return f.waitFor("text:Submitted harness@example.test")
		}},
		{name: "read", tool: "brw_read", run: func() (any, error) {
			return mgr.Read(f.tabContext())
		}},
	}
}

func (f *flowRunner) shopCommands() []commandDef {
	mgr := f.manager()
	return []commandDef{
		{name: "open", tool: "brw_open", run: f.open},
		{name: "fill_search", tool: "brw_fill", run: func() (any, error) {
			return mgr.Fill(f.tabContext(), snapshot.FillOptions{Query: "Search products", Text: "running shoes"})
		}},
		{name: "click_search", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "Search", Role: "button", Exact: true})
		}},
		{name: "find_product", tool: "brw_find", run: func() (any, error) {
			return f.findRef("product", snapshot.FindOptions{Role: "button", Text: "View Kiprun KS500", Limit: 5})
		}},
		{name: "click_product", tool: "brw_click", run: func() (any, error) {
			ref, err := f.ref("product")
			if err != nil {
				return nil, err
			}
			return mgr.Click(f.tabContext(), ref)
		}},
		{name: "find_size", tool: "brw_find", run: func() (any, error) {
			return f.findRef("size", snapshot.FindOptions{Role: "combobox", Text: "Select size", Limit: 5})
		}},
		{name: "select_size", tool: "brw_select", run: func() (any, error) {
			ref, err := f.ref("size")
			if err != nil {
				return nil, err
			}
			return mgr.Select(f.tabContext(), ref, "UK 9")
		}},
		{name: "click_add", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "Add to basket", Role: "button"})
		}},
		{name: "wait_added", tool: "brw_wait_for", run: func() (any, error) {
			return f.waitFor("text:Added Kiprun KS500 Running Shoes (UK 9) to basket")
		}},
		{name: "click_basket", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "View basket", Role: "button"})
		}},
		{name: "read", tool: "brw_read", run: func() (any, error) {
			return mgr.Read(f.tabContext())
		}},
	}
}

func (f *flowRunner) dynamicCommands() []commandDef {
	mgr := f.manager()
	return []commandDef{
		{name: "open", tool: "brw_open", run: f.open},
		{name: "click_load", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "Load delayed controls", Role: "button"})
		}},
		{name: "wait_controls", tool: "brw_wait_for", run: func() (any, error) {
			return f.waitFor("text:Save delayed notes")
		}},
		{name: "fill_delayed", tool: "brw_fill", run: func() (any, error) {
			return mgr.Fill(f.tabContext(), snapshot.FillOptions{Query: "Delayed Notes", Text: "benchmark note"})
		}},
		{name: "click_save", tool: "brw_click_text", run: func() (any, error) {
			return mgr.ClickText(f.tabContext(), snapshot.ClickTextOptions{Text: "Save delayed notes", Role: "button"})
		}},
		{name: "wait_saved", tool: "brw_wait_for", run: func() (any, error) {
			return f.waitFor("text:Saved delayed note: benchmark note")
		}},
	}
}

func (f *flowRunner) structuredCommands() []commandDef {
	mgr := f.manager()
	return []commandDef{
		{name: "open", tool: "brw_open", run: f.open},
		{name: "read_data", tool: "brw_read_data", run: func() (any, error) {
			return mgr.ReadData(f.tabContext())
		}},
		{name: "read", tool: "brw_read", run: func() (any, error) {
			return mgr.Read(f.tabContext())
		}},
		{name: "snapshot", tool: "brw_snapshot", run: func() (any, error) {
			return mgr.Snapshot(f.tabContext(), snapshot.SnapshotOptions{})
		}},
	}
}

func (f *flowRunner) fill(key, text string) (any, error) {
	ref, err := f.ref(key)
	if err != nil {
		return nil, err
	}
	return f.manager().Fill(f.tabContext(), snapshot.FillOptions{Ref: ref, Text: text, Replace: true})
}

// waitFor returns what brw_wait_for returns.
//
// It used to substitute {"condition": ...} on the grounds that a wait's result
// IS its condition holding. That was a claim about what an agent ought to be
// sent, in a column that reports what it IS sent: the tool answers with the
// whole WaitOutcome — ok, condition, resolved_by, waited_ms, wakeups — and
// resolved_by in particular is the field that tells a caller whether the
// condition it picked costs a round trip per check.
func (f *flowRunner) waitFor(condition string) (any, error) {
	outcome, err := f.manager().WaitForOutcome(f.tabContext(), condition, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wait %s: %w", condition, err)
	}
	return outcome, nil
}
