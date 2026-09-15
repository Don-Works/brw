package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

// runDaemon is a stand-in brwd: it answers /health and /api/recipes/run and
// records the interleaving of the runs it serves, which is the thing the
// serialisation test has to observe.
type runDaemon struct {
	server *httptest.Server

	mu    sync.Mutex
	trace []string

	inflight int64
	peak     int64
	runs     int64
	hold     time.Duration

	interactive bool
	// identity is the raw /health identity object. Empty uses the fixture
	// profile; a value naming none of the four profile fields stands in for a
	// daemon that will not say which browser it drives.
	identity string
	// noConsent omits the consent block entirely, which is what a daemon built
	// before /health carried one answers with.
	noConsent bool
	// respond overrides the run response. Nil answers a successful run.
	respond func(w http.ResponseWriter, body []byte)
}

func newRunDaemon(t *testing.T, d *runDaemon) *runDaemon {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		identity := d.identity
		if identity == "" {
			identity = `{"workspace":"work","profile":"chrome-work","user_data_dir":"/var/tmp/brw/chrome","profile_directory":"Profile 1","mode":"direct","transport":"direct-cdp"}`
		}
		consent := fmt.Sprintf(`,"consent":{"enabled":true,"interactive":%t}`, d.interactive)
		if d.noConsent {
			consent = ""
		}
		fmt.Fprintf(w, `{"ok":true,"identity":%s%s}`, identity, consent)
	})
	// A proxy in front of this daemon takes a tab lease before it forwards a
	// run, which opens a working tab through its controller.
	mux.HandleFunc("POST /api/browser/open", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"tab":{"id":"tab-1"},"ready":true}`)
	})
	mux.HandleFunc("POST /api/recipes/run", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		id := atomic.AddInt64(&d.runs, 1)
		now := atomic.AddInt64(&d.inflight, 1)
		for {
			high := atomic.LoadInt64(&d.peak)
			if now <= high || atomic.CompareAndSwapInt64(&d.peak, high, now) {
				break
			}
		}
		d.record(fmt.Sprintf("start %d", id))
		time.Sleep(d.hold)
		d.record(fmt.Sprintf("end %d", id))
		atomic.AddInt64(&d.inflight, -1)
		if d.respond != nil {
			d.respond(w, body)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"recipe_id":"fixture.recipe","recipe_version":"1","status":"done","steps":[{"id":"one","status":"done"}]}`)
	})
	d.server = httptest.NewServer(mux)
	t.Cleanup(d.server.Close)
	return d
}

func (d *runDaemon) record(event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trace = append(d.trace, event)
}

func (d *runDaemon) events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.trace...)
}

// isolateLocks points the run lock at a directory this test owns. The lock
// directory is deliberately not a flag — a run allowed to choose its own would
// serialise against nothing — so the test moves the whole user cache instead.
func isolateLocks(t *testing.T) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
}

func invokeRun(t *testing.T, daemon *runDaemon, extra ...string) (int, runReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"run", "fixture.recipe", "--daemon", daemon.server.URL,
		"--recipe-version", "1", "--digest", strings.Repeat("a", 64)}, extra...)
	code := Run(context.Background(), args, &stdout, &stderr)
	var report runReport
	if stdout.Len() > 0 {
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout.String())
		}
	}
	return code, report, stderr.String()
}

// TestScheduledRunsSerialiseOnOneProfile is the whole reason the lock exists: a
// scheduler fires on a clock, and two runs whose windows overlap would
// otherwise drive the same tab at the same time.
func TestScheduledRunsSerialiseOnOneProfile(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{hold: 150 * time.Millisecond})

	var wg sync.WaitGroup
	codes := make([]int, 2)
	reports := make([]runReport, 2)
	for i := range codes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			codes[index], reports[index], _ = invokeRun(t, daemon, "--lock-wait", "30s")
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != ExitOK {
			t.Fatalf("run %d exited %d: %+v", i, code, reports[i])
		}
	}
	if daemon.peak != 1 {
		t.Fatalf("the daemon served %d runs at once; they interleaved", daemon.peak)
	}
	// The trace is the evidence, not the counter: start/end must alternate, with
	// no run beginning inside another.
	events := daemon.events()
	if len(events) != 4 {
		t.Fatalf("expected two start/end pairs, got %v", events)
	}
	for i := 0; i < len(events); i += 2 {
		start, end := events[i], events[i+1]
		if !strings.HasPrefix(start, "start ") || !strings.HasPrefix(end, "end ") {
			t.Fatalf("interleaved trace: %v", events)
		}
		if strings.TrimPrefix(start, "start ") != strings.TrimPrefix(end, "end ") {
			t.Fatalf("a run ended inside another: %v", events)
		}
	}
	// Both runs serialised on the same key, which is what says they could not
	// have overlapped rather than merely happening not to.
	if reports[0].Profile.LockKey == "" || reports[0].Profile.LockKey != reports[1].Profile.LockKey {
		t.Fatalf("runs reported lock keys %q and %q", reports[0].Profile.LockKey, reports[1].Profile.LockKey)
	}
	if reports[0].LockWaitMS+reports[1].LockWaitMS < 100 {
		t.Fatalf("neither run waited for the other: %dms and %dms", reports[0].LockWaitMS, reports[1].LockWaitMS)
	}
}

// TestTwoDaemonsOnOneProfileStillSerialise: an --upstream-http proxy and the
// daemon behind it are two URLs and one browser. A lock keyed by the daemon
// would let those two interleave while each looked perfectly serialised.
func TestTwoDaemonsOnOneProfileStillSerialise(t *testing.T) {
	isolateLocks(t)
	first := newRunDaemon(t, &runDaemon{hold: 120 * time.Millisecond})
	second := newRunDaemon(t, &runDaemon{hold: 120 * time.Millisecond})

	var wg sync.WaitGroup
	var keys [2]string
	var waited int64
	for index, daemon := range []*runDaemon{first, second} {
		wg.Add(1)
		go func(index int, daemon *runDaemon) {
			defer wg.Done()
			code, report, _ := invokeRun(t, daemon, "--lock-wait", "30s")
			if code != ExitOK {
				t.Errorf("run against daemon %d exited %d", index, code)
			}
			keys[index] = report.Profile.LockKey
			atomic.AddInt64(&waited, report.LockWaitMS)
		}(index, daemon)
	}
	wg.Wait()

	if keys[0] != keys[1] || keys[0] == "" {
		t.Fatalf("the two daemons produced lock keys %q and %q for one profile", keys[0], keys[1])
	}
	if waited < 100 {
		t.Fatalf("neither run waited: the two daemons did not share a lock (total wait %dms)", waited)
	}
	if first.peak > 1 || second.peak > 1 {
		t.Fatalf("a daemon served overlapping runs: %d and %d", first.peak, second.peak)
	}
	// Each daemon ran its own recipe exactly once, so the serialisation was
	// between them rather than one of them simply not running.
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("runs served: %d and %d", first.runs, second.runs)
	}
}

// TestZeroLockWaitRefusesRatherThanQueueing gives a scheduler the other choice:
// skip this tick instead of piling up.
func TestZeroLockWaitRefusesRatherThanQueueing(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{hold: 400 * time.Millisecond})

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var stdout, stderr bytes.Buffer
		close(started)
		Run(context.Background(), []string{"run", "fixture.recipe", "--daemon", daemon.server.URL,
			"--recipe-version", "1", "--digest", strings.Repeat("a", 64), "--lock-wait", "30s"}, &stdout, &stderr)
	}()
	<-started
	// Wait until the first run is actually inside the daemon, so the second one
	// is contending rather than racing the first to the lock.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&daemon.inflight) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first run never reached the daemon")
		}
		time.Sleep(5 * time.Millisecond)
	}

	code, report, stderrText := invokeRun(t, daemon, "--lock-wait", "0")
	if code != ExitBusy {
		t.Fatalf("a contended run exited %d, want %d (busy): %+v", code, ExitBusy, report)
	}
	if report.Outcome != "busy" || !report.Retryable {
		t.Fatalf("report = %+v", report)
	}
	if report.Result != nil {
		t.Fatal("a refused run reported a result, so something was attempted")
	}
	if !strings.Contains(stderrText, "busy") {
		t.Fatalf("stderr does not say what happened: %q", stderrText)
	}
	<-done
	if daemon.runs != 1 {
		t.Fatalf("the daemon served %d runs; the refused one should never have reached it", daemon.runs)
	}
}

// TestPostconditionFailureIsDistinctFromInfrastructureFailure is the acceptance
// criterion in one test: both are non-zero, and they are different numbers.
func TestPostconditionFailureIsDistinctFromInfrastructureFailure(t *testing.T) {
	isolateLocks(t)
	failing := newRunDaemon(t, &runDaemon{respond: func(w http.ResponseWriter, _ []byte) {
		w.Header().Set(usagelog.HeaderErrorClass, "tool")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"recipe_id":"fixture.recipe","status":"failed","steps":[{"id":"submit","status":"failed"}],"error":"step \"submit\": postcondition network_response did not occur"}`)
	}})
	code, report, stderrText := invokeRun(t, failing)
	if code != ExitPostconditionFailed {
		t.Fatalf("a failed postcondition exited %d, want %d: %+v", code, ExitPostconditionFailed, report)
	}
	if report.Outcome != "postcondition_failed" || report.OK {
		t.Fatalf("report = %+v", report)
	}
	if report.Result == nil || report.Result.Status != "failed" {
		t.Fatalf("the report dropped the run result: %+v", report.Result)
	}
	if !strings.Contains(stderrText, "postcondition") {
		t.Fatalf("stderr does not name the failure: %q", stderrText)
	}

	// Infrastructure: the daemon is not there at all.
	var stdout, stderr bytes.Buffer
	infraCode := Run(context.Background(), []string{"run", "fixture.recipe",
		"--daemon", "http://127.0.0.1:1", "--recipe-version", "1", "--digest", strings.Repeat("a", 64)},
		&stdout, &stderr)
	var infra runReport
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &infra); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%s)", err, stdout.String())
	}
	if infraCode != ExitNoDaemon || infra.Outcome != "infrastructure" {
		t.Fatalf("an unreachable daemon exited %d (%s), want %d", infraCode, infra.Outcome, ExitNoDaemon)
	}
	if infraCode == code {
		t.Fatalf("a postcondition failure and an infrastructure failure both exit %d", code)
	}
	if !infra.Retryable {
		t.Fatal("an unreachable daemon is worth retrying, and the report says it is not")
	}
}

// TestPolicyRefusalGetsItsOwnExitCode: a run refused by site permissions must
// not read as a broken daemon. Retrying it forever is the failure that causes.
func TestPolicyRefusalGetsItsOwnExitCode(t *testing.T) {
	isolateLocks(t)
	refusing := newRunDaemon(t, &runDaemon{respond: func(w http.ResponseWriter, _ []byte) {
		// The class the real daemon attaches to a consent refusal; see
		// internal/http's own test that it does.
		w.Header().Set(usagelog.HeaderErrorClass, "policy_denied")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"no site permission grant for https://shop.test (scope act)"}`)
	}})
	code, report, _ := invokeRun(t, refusing)
	if code != ExitPolicyRefused {
		t.Fatalf("a refused run exited %d, want %d: %+v", code, ExitPolicyRefused, report)
	}
	if report.Outcome != "policy_refused" || report.Retryable {
		t.Fatalf("a policy refusal must not be advertised as retryable: %+v", report)
	}
	if report.ErrorClass != "policy_denied" {
		t.Fatalf("the report lost the daemon's classification: %q", report.ErrorClass)
	}
	if report.Result != nil {
		// A refusal never reached the runner, so there is no run to report. A
		// zero RunResult here would put a started_at of year 1 into a
		// scheduler'''s ledger as though a run had been attempted.
		t.Fatalf("a refused run carried a run result: %+v", report.Result)
	}
}

// TestRunRefusesADaemonThatCouldPrompt is the fail-closed rule. A daemon with a
// prompter blocks on a terminal read nobody will answer, so the scheduled run
// that was meant to refuse would hang to its timeout and report a timeout.
func TestRunRefusesADaemonThatCouldPrompt(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{interactive: true})
	code, report, stderrText := invokeRun(t, daemon)
	if code != ExitPolicyRefused {
		t.Fatalf("a prompting daemon exited %d, want %d: %+v", code, ExitPolicyRefused, report)
	}
	if daemon.runs != 0 {
		t.Fatalf("the run reached the daemon %d times; it must refuse before starting", daemon.runs)
	}
	if !strings.Contains(report.Error, "site-consent-prompt") || !strings.Contains(report.Error, "brwctl grants allow") {
		t.Fatalf("the refusal does not tell the operator what to change: %q", report.Error)
	}
	if !strings.Contains(stderrText, "policy_refused") {
		t.Fatalf("stderr does not name the outcome: %q", stderrText)
	}
}

// TestRunReportsOnStdoutAndDiagnosticsOnStderr pins the half of the contract a
// scheduler's wrapper depends on: stdout is machine-readable and nothing else.
func TestRunReportsOnStdoutAndDiagnosticsOnStderr(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{})
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"run", "fixture.recipe", "--daemon", daemon.server.URL,
		"--recipe-version", "1", "--digest", strings.Repeat("a", 64), "--input", "month=2026-09"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if lines := strings.Count(strings.TrimSpace(stdout.String()), "\n"); lines != 0 {
		t.Fatalf("stdout carries %d extra lines; it must be exactly one JSON object:\n%s", lines, stdout.String())
	}
	var report runReport
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if report.Schema != runSchema {
		t.Fatalf("schema = %q, want %q", report.Schema, runSchema)
	}
	switch {
	case !report.OK, report.Outcome != "ok", report.ExitCode != ExitOK:
		t.Fatalf("report = %+v", report)
	case report.Recipe.ID != "fixture.recipe" || report.Recipe.Version != "1":
		t.Fatalf("report does not name what it ran: %+v", report.Recipe)
	case report.Profile.Profile != "chrome-work":
		t.Fatalf("report does not name the profile it ran against: %+v", report.Profile)
	case report.Result == nil || report.Result.Status != "done":
		t.Fatalf("report does not carry the run result: %+v", report.Result)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr says nothing, so a scheduler's log records no diagnostics at all")
	}
	if strings.Contains(stderr.String(), `"schema"`) {
		t.Fatal("the JSON contract leaked onto stderr as well")
	}
}

// TestRunRefusesAnUnpinnedRecipe: version and digest are what tie a scheduled
// job to one immutable recipe. Without them the job silently starts running
// whatever was published last.
func TestRunRefusesAnUnpinnedRecipe(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{})
	for _, args := range [][]string{
		{"run", "fixture.recipe", "--daemon", daemon.server.URL, "--digest", strings.Repeat("a", 64)},
		{"run", "fixture.recipe", "--daemon", daemon.server.URL, "--recipe-version", "1"},
		{"run", "--daemon", daemon.server.URL, "--recipe-version", "1", "--digest", strings.Repeat("a", 64)},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != ExitUsage {
			t.Errorf("%v exited %d, want %d", args, code, ExitUsage)
		}
		if daemon.runs != 0 {
			t.Fatalf("a rejected invocation still reached the daemon")
		}
	}
}

// TestRunOnlyReachesARouteTheDaemonServes keeps the CLI invariant: `brw run`
// may not have a surface internal/http does not.
func TestRunOnlyReachesARouteTheDaemonServes(t *testing.T) {
	isolateLocks(t)
	routes := daemonRoutes(t)
	if _, ok := routes["POST /api/recipes/run"]; !ok {
		t.Fatal("internal/http does not register POST /api/recipes/run, which brw run drives")
	}

	// And behaviourally: a daemon serving only /health and that one route
	// answers the whole command, so nothing else was reached.
	var reached []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/health":
			fmt.Fprint(w, `{"ok":true,"identity":{"profile":"p"},"consent":{"enabled":false}}`)
		case r.URL.Path == "/api/recipes/run":
			fmt.Fprint(w, `{"recipe_id":"fixture.recipe","status":"done","steps":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"run", "fixture.recipe", "--daemon", server.URL,
		"--recipe-version", "1", "--digest", strings.Repeat("a", 64)}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, path := range reached {
		if path != "GET /health" && path != "POST /api/recipes/run" {
			t.Errorf("brw run reached %s, which is not part of its binding", path)
		}
	}
}

// TestEveryRunOutcomeIsDistinctAndDocumented enumerates the exit-code contract.
// The exit code is the entire interface a scheduler has, so two outcomes
// sharing a code, or an outcome the code produces that the table never
// describes, is an operator's wrapper reading the wrong thing.
func TestEveryRunOutcomeIsDistinctAndDocumented(t *testing.T) {
	byName := map[string]runOutcome{}
	byCode := map[int]string{}
	for _, row := range runOutcomes {
		if _, duplicate := byName[row.Name]; duplicate {
			t.Errorf("outcome %q is listed twice", row.Name)
		}
		if other, duplicate := byCode[row.Code]; duplicate {
			t.Errorf("outcomes %q and %q both exit %d, so a scheduler cannot tell them apart", other, row.Name, row.Code)
		}
		if strings.TrimSpace(row.Meaning) == "" {
			t.Errorf("outcome %q has no documented meaning", row.Name)
		}
		byName[row.Name] = row
		byCode[row.Code] = row.Name
	}
	if byName["ok"].Code != ExitOK {
		t.Error("the ok outcome does not exit 0")
	}
	for name, row := range byName {
		if name != "ok" && row.Code == ExitOK {
			t.Errorf("outcome %q exits 0, which a scheduler reads as success", name)
		}
	}

	// Every outcome name the code actually produces has to be in the table. The
	// names are read out of run.go rather than listed here: a classification
	// added with a name nobody registered would fall through to the unclassified
	// default and exit 1.
	used := outcomeNamesInSource(t, "run.go")
	if len(used) < len(runOutcomes)-1 {
		t.Fatalf("found only %d outcome names in run.go; the scan is not reading the source", len(used))
	}
	for _, name := range used {
		if _, known := byName[name]; !known {
			t.Errorf("run.go classifies a run as %q, which runOutcomes does not list", name)
		}
	}
	for name := range byName {
		if name != "ok" && !contains(used, name) {
			t.Errorf("runOutcomes lists %q, which nothing in run.go ever produces", name)
		}
	}

	// The usage text an operator reads is generated from the same table, so it
	// cannot describe a code the binary does not return.
	var help bytes.Buffer
	runUsageText(&help)
	for _, row := range runOutcomes {
		if !strings.Contains(help.String(), fmt.Sprintf("%d  %s", row.Code, row.Name)) {
			t.Errorf("brw run --help does not document exit %d (%s)", row.Code, row.Name)
		}
	}
}

// outcomeNamesInSource returns every string literal passed to outcome() or as
// the outcome argument of failRun() in the named file.
func outcomeNamesInSource(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var names []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		index := -1
		switch fn.Name {
		case "outcome":
			index = 0
		case "failRun":
			index = 2
		default:
			return true
		}
		if index >= len(call.Args) {
			return true
		}
		literal, ok := call.Args[index].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		name := strings.Trim(literal.Value, `"`)
		if !contains(names, name) {
			names = append(names, name)
		}
		return true
	})
	return names
}

// failingRunDaemon answers one failed run, with the error class the argument
// names. The class is the daemon's own classification of the innermost failure,
// which is the thing brw run has to weigh against the structured result.
func failingRunDaemon(t *testing.T, class string) *runDaemon {
	t.Helper()
	return newRunDaemon(t, &runDaemon{respond: func(w http.ResponseWriter, _ []byte) {
		if class != "" {
			w.Header().Set(usagelog.HeaderErrorClass, class)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"recipe_id":"fixture.recipe","status":"failed","steps":[{"id":"submit","status":"failed"}],"error":"step \"submit\": postcondition network_response did not occur"}`)
	}})
}

// TestAFailedStepIsAPostconditionFailureWhateverClassTheDaemonAttached is the
// acceptance criterion held against the classes the daemon really sends.
//
// Every postcondition the recipe compiler infers by default is a wait, and a
// wait that ran out is classified "timeout" — which usagelog.Retryable says is
// worth another attempt, which used to return "infrastructure" and exit 3, the
// same code as an unreachable daemon. The whole point of the exit-code contract
// is that 4 and 3 are different answers, so the structured result outranks the
// class rather than the other way round.
func TestAFailedStepIsAPostconditionFailureWhateverClassTheDaemonAttached(t *testing.T) {
	// Every class the daemon computes that Retryable() says yes to, plus the two
	// it reaches for a postcondition in practice and the empty one.
	classes := []string{"", "tool", "timeout", "busy", "transport", "takeover_held", "target_not_found"}
	for _, class := range classes {
		t.Run("class "+class, func(t *testing.T) {
			isolateLocks(t)
			daemon := failingRunDaemon(t, class)
			code, report, stderrText := invokeRun(t, daemon)
			if code != ExitPostconditionFailed {
				t.Fatalf("a failed step with class %q exited %d, want %d: %+v", class, code, ExitPostconditionFailed, report)
			}
			if report.Outcome != "postcondition_failed" || report.OK {
				t.Fatalf("report = %+v", report)
			}
			if report.Result == nil || report.Result.Status != "failed" {
				t.Fatalf("the report dropped the run result: %+v", report.Result)
			}
			if !strings.Contains(stderrText, "postcondition") {
				t.Fatalf("stderr does not name the failure: %q", stderrText)
			}
		})
	}

	// And the other direction: the same retryable class with no failed step is
	// still an infrastructure failure, so this did not simply delete the arm.
	isolateLocks(t)
	noResult := newRunDaemon(t, &runDaemon{respond: func(w http.ResponseWriter, _ []byte) {
		w.Header().Set(usagelog.HeaderErrorClass, "timeout")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"no response from downstream"}`)
	}})
	code, report, _ := invokeRun(t, noResult)
	if code != ExitNoDaemon || report.Outcome != "infrastructure" {
		t.Fatalf("a retryable class with no run result exited %d (%s), want %d (infrastructure)", code, report.Outcome, ExitNoDaemon)
	}

	// A refusal stays a refusal even when the run got far enough to fail a step:
	// retrying it changes nothing until a human grants something.
	isolateLocks(t)
	refused := failingRunDaemon(t, "policy_denied")
	code, report, _ = invokeRun(t, refused)
	if code != ExitPolicyRefused || report.Retryable {
		t.Fatalf("a refusal carrying a failed step exited %d (retryable=%v), want %d and not retryable", code, report.Retryable, ExitPolicyRefused)
	}
}

// TestRunRefusesADaemonThatReportsNoConsentPosture is the fail-closed rule held
// against a daemon older than the block it reads. The Consent field decodes to
// its zero value when /health sends nothing, so "this daemon has no prompter"
// and "this daemon said nothing" used to be the same answer — and the second
// one is exactly the daemon brw cannot see into.
func TestRunRefusesADaemonThatReportsNoConsentPosture(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{noConsent: true})
	code, report, stderrText := invokeRun(t, daemon)
	if code != ExitPolicyRefused {
		t.Fatalf("a daemon reporting no consent posture exited %d, want %d: %+v", code, ExitPolicyRefused, report)
	}
	if daemon.runs != 0 {
		t.Fatalf("the run reached the daemon %d times; it must refuse before starting", daemon.runs)
	}
	if !strings.Contains(report.Error, "consent posture") {
		t.Fatalf("the refusal does not say what was missing: %q", report.Error)
	}
	if !strings.Contains(stderrText, "policy_refused") {
		t.Fatalf("stderr does not name the outcome: %q", stderrText)
	}
}

// A daemon that names no profile at /health takes the key every anonymous
// daemon shares. That serialises it against other anonymous daemons but NOT
// against an identified daemon on the same Chrome, which takes the profile's
// own key.
//
// It used to be refused outright, and that broke the default install: a daemon
// started without --workspace/--profile is what `brwd` on its own is, and `brw
// run` against one exited 1 every time. The gap is reported instead — in the
// JSON a scheduler parses and on stderr a human reads — because a run that
// cannot start at all is worse than one that says which guarantee it has.
func TestRunAgainstAnAnonymousDaemonRunsAndReportsTheSharedLock(t *testing.T) {
	for _, identity := range []string{`{}`, `{"mode":"direct","transport":"direct-cdp"}`} {
		t.Run(identity, func(t *testing.T) {
			isolateLocks(t)
			daemon := newRunDaemon(t, &runDaemon{identity: identity})
			code, report, stderrText := invokeRun(t, daemon)
			if code != ExitOK {
				t.Fatalf("a run against a daemon started without --workspace/--profile exited %d: %+v\n%s", code, report, stderrText)
			}
			if daemon.runs != 1 {
				t.Fatalf("the daemon served %d runs", daemon.runs)
			}
			if !report.Profile.LockShared {
				t.Fatalf("the report does not say the lock was the shared one, so a scheduler cannot tell which guarantee it got: %+v", report.Profile)
			}
			if report.Profile.LockKey == "" {
				t.Fatalf("the report names no lock key at all: %+v", report.Profile)
			}
			if !strings.Contains(stderrText, "--workspace/--profile") {
				t.Fatalf("stderr does not tell the operator how to get a lock keyed on the profile: %q", stderrText)
			}
		})
	}
}

// And the report is not decoration: a run through a daemon that DOES name its
// profile must not be labelled the same way, or the field says nothing.
func TestAnIdentifiedDaemonIsNotReportedAsSharingTheAnonymousLock(t *testing.T) {
	isolateLocks(t)
	daemon := newRunDaemon(t, &runDaemon{})
	code, report, _ := invokeRun(t, daemon)
	if code != ExitOK {
		t.Fatalf("exited %d: %+v", code, report)
	}
	if report.Profile.LockShared {
		t.Fatalf("a daemon naming workspace, profile and user data dir was reported as anonymous: %+v", report.Profile)
	}
}

// The guarantee the shared key does carry, which is why the run is allowed to
// proceed on it: two anonymous daemons still queue behind each other rather
// than driving one browser at once.
func TestAnonymousDaemonsStillSerialiseAgainstEachOther(t *testing.T) {
	isolateLocks(t)
	const anonymous = `{"mode":"direct","transport":"direct-cdp"}`
	first := newRunDaemon(t, &runDaemon{hold: 120 * time.Millisecond, identity: anonymous})
	second := newRunDaemon(t, &runDaemon{hold: 120 * time.Millisecond, identity: anonymous})

	var wg sync.WaitGroup
	var waited int64
	for index, daemon := range []*runDaemon{first, second} {
		wg.Add(1)
		go func(index int, daemon *runDaemon) {
			defer wg.Done()
			code, report, _ := invokeRun(t, daemon, "--lock-wait", "30s")
			if code != ExitOK {
				t.Errorf("run against anonymous daemon %d exited %d", index, code)
			}
			atomic.AddInt64(&waited, report.LockWaitMS)
		}(index, daemon)
	}
	wg.Wait()

	if waited < 100 {
		t.Fatalf("neither run waited, so the two anonymous daemons did not share a lock (total wait %dms)", waited)
	}
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("runs served: %d and %d", first.runs, second.runs)
	}
}
